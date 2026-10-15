# Threat model di Nexo (STRIDE)

Questo documento estende la sintesi di [docs/07 §7.1](../07-sicurezza-operativita.md#71-threat-model-sintesi-stride)
con un'analisi STRIDE per superficie (M9 T-9.3). Per ogni minaccia indica la mitigazione **nel codice** (file e
meccanismo) e il **rischio residuo**: ciò che resta a carico del deployment o è accettato. I riferimenti sono relativi
alla radice del repository; le scansioni automatiche (govulncheck, gosec, npm audit) e il fuzzing sono descritti in
§10 e §11, i rischi residui consolidati in §12.

## 1. Perimetro, asset e confini di fiducia

**Asset**: dati e metadati degli oggetti (anche cifrati), credenziali (root, utenti IAM, service account, sessioni
STS, token della console), chiavi (master key del KMS, DEK, chiavi SSE-C dei client, chiavi di firma delle
distribuzioni, segreti di origine dei proxy), configurazione (IAM, policy, regole di accesso, distribuzioni, cache,
target degli eventi), disponibilità del servizio e integrità del cluster (formato dei drive, quorum, lock
distribuiti).

**Attori**: client anonimi su Internet (website, link firmati, oggetti pubblici); client S3 autenticati (utenti,
service account, sessioni STS); amministratori (console, Admin API, `nexoctl`); altri nodi del cluster; servizi esterni
chiamati da Nexo (KES, target degli eventi, webhook di purge, ACME); l'operatore della macchina (filesystem, variabili
d'ambiente, segnali).

**Confini di fiducia**:

| Confine | Dentro | Fuori |
|---------|--------|-------|
| Listener S3 (`--address`, anche domini delle distribuzioni su opt-in) e listener web delle distribuzioni (`--web-address`, `--web-tls-address`) | handler, servizi | qualunque client di rete |
| Listener console/Admin API (`--console-address`) | API `/api/v1`, SPA | browser e `nexoctl` |
| RPC internode (`/nexo/rpc/v1/...` sul listener S3) | nodi con le stesse credenziali root e lo stesso deployment id | ogni altro peer |
| Egress (KES, eventi, webhook, ACME) | Nexo | servizi esterni, anche di terze parti |
| Filesystem dei drive, `--certs-dir`, `.nexo.sys` | processo `nexo` (utente dedicato) | altri utenti locali |

**Assunzioni**: l'utente di sistema che esegue `nexo` e chi ne controlla l'ambiente (variabili, file di
configurazione, `--certs-dir`) sono fidati; i drive non sono scrivibili da altri utenti; il root dell'istanza è un
amministratore fidato (bypassa le policy per progetto). I reverse proxy davanti a Nexo sono fidati solo se elencati
in `--trusted-proxies`.

## 2. API S3 (`internal/transport/http/s3`, `internal/service/auth`, `internal/service/policy`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | Firma falsificata o riutilizzata | SigV4 completo con confronto a tempo costante e cache delle chiavi derivate (`internal/service/auth/sigv4.go`, `keycache.go`); finestra di clock skew (`RequestTimeTooSkewed`, `errors.go`); presigned al massimo 7 giorni (`canonical.go` `maxPresignExpiry`); payload firmato o `UNSIGNED-PAYLOAD` esplicito, chunk firmati verificati uno per uno (`chunked.go`); POST policy con condizioni verificate (`postpolicy.go`); scope del servizio (`scope.go`) | Replay di una richiesta firmata dentro la finestra di skew (come in S3): usare TLS |
| S | Credenziali STS rubate | Sessioni a scadenza, token legato alla chiave (`internal/service/iam/sts.go`); revoca alla rotazione dei segreti | Un token rubato vale fino alla scadenza |
| T | Corpo alterato in transito o su disco | Content-MD5 e checksum `x-amz-checksum-*` (`internal/transport/http/s3/body.go`, `internal/service/auth/checksum.go`, `internal/service/object/hash.go`); hash bitrot per blocco HighwayHash (`internal/service/erasure/bitrot.go`); CRC di `xl.meta` (`internal/service/storage/xlmeta.go`); TLS | Senza TLS i metadati non firmati (header non inclusi in `SignedHeaders`) sono modificabili da un MITM |
| T | Path traversal nelle chiavi (`../`, NUL, UTF-8 invalido) | Validazione delle chiavi (`internal/domain/object.go`), percorsi risolti dentro il drive con `O_NOFOLLOW` (`internal/service/storage/fsutil.go`), normalizzazione condivisa con le regole di accesso (`internal/service/match/normalize.go`) | — |
| T | Sovrascrittura di versioni protette | Object Lock (retention, legal hold, bypass governance solo con permesso esplicito, `internal/service/object/buckets.go`), versioning | Il root può cancellare un bucket in governance con bypass, per progetto |
| R | Operazioni non attribuibili | Request id su ogni risposta (`middleware/requestid.go`), audit log con identità (`middleware/audit.go`, `internal/service/audit`) | La consegna dell'audit ai target esterni è asincrona: un crash può perdere gli eventi non ancora consegnati |
| I | Lettura non autorizzata | Policy deny-by-default valutate server-side per ogni operazione (`internal/service/policy`, `internal/transport/http/s3/authz.go`), regole di accesso glob/regex e Block Public Access (`internal/service/accessrule`), messaggi d'errore generici agli anonimi | Una bucket policy permissiva scritta dall'amministratore è rispettata |
| I | Dati a riposo leggibili da chi ha i drive | SSE-S3/SSE-KMS/SSE-C, stream DARE con anti-troncamento (`internal/service/crypto/stream.go`), chiavi d'oggetto sigillate (`internal/service/crypto/crypto.go`) | Oggetti non cifrati sono in chiaro sui drive; i metadati (nomi, dimensioni) non sono cifrati |
| D | Corpi/documenti enormi, XML annidato, chunk malformati; client che smettono di leggere (socket non svuotato, finestra HTTP/2 a zero) per tenere occupati handler, lock e buffer | Limiti dei documenti XML/JSON (`maxXMLBody`, `maxCompleteBody` in `s3/body.go`, `s3/multipart.go`), limite del corpo (`middleware/maxbody.go`), `ReadHeaderTimeout` e timeout dei listener (`internal/transport/http/server/server.go`), cifre massime della dimensione dei chunk (`auth/chunked.go`), quote dei bucket (`internal/service/quota`); limite di stallo su ogni scrittura di ogni risposta di ogni listener (`server/stall.go`, `NEXO_SEND_TIMEOUT`, 20 s: la risposta HTTP/1 o lo stream HTTP/2 viene interrotto) e su ogni connessione HTTP/2, chiusa con tutti i suoi stream quando non accetta byte per lo stesso tempo (`HTTP2Config.WriteByteTimeout` in `server.go`: il reset di uno stream non può partire se il client non svuota il socket); handler, lock e corpo bufferizzato vengono così rilasciati; finestre HTTP/2 di net/http (1 MiB per connessione) e al più 128 stream per connessione, finestre maggiori (8 MiB per stream, 16 MiB per connessione) solo sul listener S3 di un cluster, che trasporta l'RPC (`server.WithHTTP2Windows`) | Nessun rate limit per client sull'API S3: un client (anche anonimo verso oggetti pubblici) può saturare banda e CPU; usare un reverse proxy con limiti (`deploy/proxy`). Vulnerabilità della toolchain (§10.1) |
| E | Escalation da utente a root | Le policy non concedono azioni `admin:*` implicite; le credenziali derivate (service account, sessioni STS) valgono per l'intersezione tra la policy inline e quelle del padre, root compreso (`internal/service/policy/evaluate.go`); un utente gestisce solo le proprie chiavi e una credenziale derivata nessuna, così non può coniarne una più ampia (`internal/service/iam/ownership.go`) | — |

## 3. Admin API e console (`internal/transport/http/admin`, `internal/service/admin`, `internal/service/session`, `web/`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | Furto della sessione, login forzato | Cookie di sessione `HttpOnly`, `SameSite=Strict`, `Secure` se la connessione (o il proxy fidato) è TLS (`admin/session.go`); chiave di sessione HKDF(root, deployment id) (`internal/service/session/session.go`); throttling dei login per utente e per client (`session/throttle.go`); SigV4 per i client programmatici | Senza TLS il cookie viaggia in chiaro |
| S | CSRF | Header CSRF obbligatorio per le richieste con cookie, controllo di origine anche cross-port (`middleware/sessionauth.go`) | — |
| T | Richieste manipolate (JSON) | Decodifica stretta (campi sconosciuti rifiutati, limiti di dimensione), validazione nei servizi; fuzzing end-to-end (`test/fuzz/admin_test.go`) | — |
| R | Azioni amministrative non tracciate | Audit di ogni richiesta con identità e azione | Vedi §2 |
| I | Dettagli interni negli errori | Il `detail` dei problem document è un messaggio curato per classe di errore, o la causa che il servizio formula per il chiamante (campo non valido, permesso mancante); la catena interna (package, percorsi, dischi) va solo nel log della richiesta con il request id (`httpx/errors.go`, `middleware/logging.go`); un errore sconosciuto è un 500 senza dettaglio | — |
| I | Fuga di segreti dalla console | Secret key mostrata una volta; valori sensibili mascherati in `GET /config`; CSP restrittiva della SPA (`internal/transport/http/console/spa.go`); download con cookie marcatore senza segreti (`admin/objects.go`) | Un profilo pprof (`POST /api/v1/profile`) contiene stack e dati di memoria: è riservato ad `admin:Profiling` (root, policy `diagnostics`) |
| D | Profiling/speedtest come vettore di carico | Un profilo alla volta (409), durate 1s–2m, speedtest con limiti di dimensione/durata/concorrenza e 1 GiB per target (`internal/service/admin/diagnostics.go`); azioni dedicate `admin:Profiling`, `admin:Speedtest` | Uno speedtest autorizzato carica i drive e la rete per la sua durata |
| E | Route senza autorizzazione | Ogni route dichiara le azioni (`x-nexo-actions` in `api/openapi.yaml`), guard centralizzato (`admin/authz.go`), test di coerenza route ↔ spec (`admin/routes_test.go` `TestRouteTableMatchesSpec`); operazioni sugli oggetti con le stesse condition key dell'API S3 (`internal/service/policy/request.go`) | — |

## 4. RPC internode (`internal/transport/rpc`, `internal/service/cluster`, `internal/app/foundation.go`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | Nodo estraneo che si finge peer | JWT HS256 con chiave HKDF(credenziali root, deployment id), audience `nexo-internode`, TTL 20s, legati a metodo, target e hash degli argomenti (`rpc/auth.go`); mTLS opzionale (`--tls-internode-verify`: certificato client dal keystore, `internodeTLS` in `app/foundation.go`; verificato solo con le CA di `CAs/` o il certificato condiviso, mai con gli intermedi di `public.crt`, e legato a un host degli endpoint dei nodi: `certs/keystore.go`, `rpc.WithClientCertHosts`) | Chi conosce le credenziali root può impersonare un nodo: proteggerle come chiavi del cluster |
| T | Frame alterati | TLS; CRC32 per frame (`rpc/frame.go`) | Senza TLS i dati tra nodi sono in chiaro e modificabili (il CRC non è un MAC) |
| R | — | Log delle chiamate fallite | — |
| I | Lettura dei dati tra nodi | TLS tra nodi (keystore di `--certs-dir`, CA in `CAs/`) | Il pool delle CA *client* (verifica dei peer) è ricaricato a caldo, quello *server* usato dai client RPC è letto all'avvio: una nuova CA dei peer richiede un riavvio (docs/07 §7.3) |
| D | Flood di autenticazioni fallite | Limiter dei fallimenti per indirizzo con tabella limitata (`rpc/limiter.go`); limiti di dimensione dei messaggi | Il limiter è per processo, fail-open oltre la tabella |
| E | Esecuzione di operazioni non previste | Solo route registrate (`rpc/routes.go`, `storage_routes.go`), argomenti msgpack tipizzati | — |

## 5. Distribuzioni e website (`internal/service/distribution`, `internal/transport/http/dist`, `internal/service/certs`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | Dominio altrui agganciato (alias takeover), Host spoofing | Alias univoci e validati (`distribution/validate.go`, `routing.go`). Sono **riservati** (esatti e sotto una wildcard, rifiutati al salvataggio ed esclusi dalla tabella di routing se salvati prima) i domini di `--server-url`/`--browser-redirect-url`, `--domain` e i suoi virtual host, gli host degli endpoint dei nodi, gli host di `--address`/`--console-address`, `localhost` e il nome della macchina (`app/distribution.go` `deploymentHosts`). Il listener S3 non instrada mai per Host i percorsi riservati `/nexo/` (health, metriche, RPC internode: `dist.WithPassthrough`, `s3` `hostBucket`, RPC senza skip per le distribuzioni in `app/foundation.go`) e serve gli alias solo su opt-in (`--s3-distributions`: `auto` quando `--server-url` o `--domain` dichiarano i nomi dell'endpoint S3, `on`, `off`); i listener web (`--web-address`, `--web-tls-address`) servono solo distribuzioni. Un alias (esatto o wildcard più specifica) sotto la wildcard di un'altra distribuzione richiede `admin:ManageDistributions` anche sul bucket di quella (`admin/distributions.go` `authorizeCovered`). Header forwarded solo da proxy fidati (`middleware/forwarded.go`); segreto di origine per i proxy (`deploy/proxy`). Ripristino senza API funzionante: `nexo server --no-distributions` (nessun host instradato, distribuzioni modificabili) | DNS e proxy sono responsabilità dell'operatore; con `--s3-distributions on` (o con `auto` e altri nomi DNS che raggiungono la porta S3 oltre a quelli dichiarati) un alias può coincidere con un nome usato dai client S3: dichiarare tutti i nomi in `--server-url`/`--domain` o servire le distribuzioni sui listener web |
| S | URL firmati contraffatti | Firme Ed25519/RSA per key group con scadenza e vincoli (`distribution/signing.go`, `keygroups.go`); fuzz di `VerifySignedRequest` | Una chiave privata di un key group rubata firma URL validi fino alla rotazione |
| T | Path traversal, doppia codifica, Unicode | Normalizzazione unica delle chiavi (`match/normalize.go`: NFC, `.`/`..`), fuzz `FuzzWebsitePath`, `FuzzNormalize` | — |
| I | Accesso a oggetti privati via website | Comportamenti per path (`access: public|signed|authenticated|deny`), Block Public Access, index/error page senza oracolo di esistenza | — |
| D | Richieste costose sui domini pubblici | Cache per dominio (§6), collapsing delle richieste | Vedi rate limit (§2) |
| E | Certificato TLS caricato da un utente non admin | Upload riservato ad `admin:ManageCertificates`; chiave privata mai restituita | — |
| T/I | Certificati del listener principale | Keystore di `--certs-dir` con ricarica atomica (fsnotify + SIGHUP), file limitati a 1 MiB, avviso per chiavi leggibili da altri, TLS ≥ 1.2 con suite AEAD/ECDHE (`internal/service/certs/keystore.go`, `internal/transport/http/server/tls.go`) | Un file illeggibile mantiene i certificati precedenti (log `error`, metrica `tls_server_certificate_reloads_total{result="error"}`) |

## 6. Cache dei domini (`internal/service/cache`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | Cache poisoning / risposta di un utente servita a un altro | Chiave non ambigua che include identità e varianti (`cache/key.go`), bypass per richieste autenticate/cookie configurabili | Una policy di cache che ignora header rilevanti è una scelta dell'amministratore |
| T | Voci su disco alterate | CRC del corpo e dei metadati, verifica di dimensione e di chiave a ogni lettura (`cache/disk.go`); rimozioni confinate alla directory con `os.Root` | — |
| I | Copie dei dati in cache leggibili da altri utenti locali | File delle voci e coda dei webhook con permessi `0600` (directory `0750`) | Il tier L2 contiene il **chiaro** degli oggetti serviti dalle distribuzioni, anche se cifrati SSE-S3/KMS sui drive: mettere la cache su un volume cifrato o non abilitare L2 per contenuti sensibili |
| D | Invalidazione massiva, webhook lenti | Code persistenti con backoff, limiti di concorrenza, flight singolo per chiave (`flight.go`) | — |
| E | SSRF tramite i webhook di purge | Politica di egress sull'indirizzo effettivamente connesso (dopo la risoluzione DNS: anche il rebinding è bloccato), niente redirect né proxy, mai i nodi del cluster, reti private solo se in `--cache-webhook-allow`; segreti solo dal namespace `NEXO_SECRET_WEBHOOK_*` (`cache/egress.go`) | — |
| R | Purge non attribuibili | Invalidazioni con id e audit | — |

## 7. Eventi e webhook (`internal/service/event`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | Destinatario impersonato | TLS verificato per i target; `tls_skip_verify` è un opt-in esplicito per target (`event/factory.go`) | Un target configurato con `tls_skip_verify` accetta un MITM |
| T | Iniezione SQL nel target PostgreSQL | Statement statici con argomenti separati; il protocollo semplice (interpolazione lato client, GO-2026-5004 di pgx < 5.9.2) è sostituito dal protocollo esteso (`event/dialers.go`) | Vedi §10.1 per l'aggiornamento di pgx |
| I | Credenziali nei log o negli errori | URL redatti (user info e query) negli errori e nello stato dei target (`event/webhook.go`); nessun redirect seguito | — |
| D | Target lento o irraggiungibile | Coda persistente (`queue_dir`) con limite e retry (`event/queue.go`) | A coda piena i nuovi eventi del target sono rifiutati (errore registrato) finché la coda non si svuota |
| E | Target come SSRF | I target sono definiti solo nella configurazione del server (`admin:ConfigUpdate`); le regole dei bucket possono solo riferirli per ARN | Un amministratore può puntare un target a servizi interni (scelta consapevole) |

## 8. KMS e cifratura (`internal/service/kms`, `internal/service/crypto`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | KES falso | Endpoint solo `https`, niente redirect, certificato client (`kms/kes.go`) | Senza certificato client il KES accetta chiunque abbia l'API key (avviso all'avvio) |
| T | Chiavi sigillate alterate | AES-GCM per sigillare la chiave d'oggetto con KEK derivata via HKDF da un sale casuale per sigillatura (`crypto/crypto.go`); stream DARE autenticato con anti-troncamento (`crypto/stream.go`); chiavi di parte derivate in modo iniettivo (`crypto/key.go`, numero di parte ≤ 2³²−1) | — |
| I | Master key esposta | Master key solo da env/file (`NEXO_KMS_SECRET_KEY`), mai nei log (tipi redatti); keystore su file `0600` in directory `0700` (`kms/keystore.go`); segreti IAM cifrati a riposo con Argon2id + AES-GCM (`internal/service/iam/crypto.go`) | Chi legge l'ambiente del processo legge la master key del KMS builtin: preferire KES in produzione |
| D | KMS irraggiungibile | Errori espliciti, nessun fallback in chiaro | Gli oggetti SSE-KMS non sono leggibili finché il KMS non torna |
| E | Chiave SSE-C di un altro client | La chiave SSE-C non è salvata; verifica con HMAC salato (`crypto.go` `ssecHMAC`) | — |

## 9. CLI `nexoctl` (`internal/cli`)

| STRIDE | Minaccia | Mitigazione (codice) | Rischio residuo |
|--------|----------|----------------------|-----------------|
| S | Server falso | Verifica TLS sempre attiva (CA del sistema), nessuna opzione per disattivarla | Un alias `http://` invia le richieste in chiaro (firmate, ma leggibili) |
| T | Zip-slip / scrittura fuori dalla destinazione | `JoinTarget` rifiuta percorsi che escono dalla directory (`internal/cli/location.go`), scritture atomiche (`transfer.go`) | — |
| I | Credenziali dell'alias leggibili | File di configurazione `0600` in directory `0700`, avviso se più permissivi; export di configurazioni (regole, distribuzioni, cache) scritti `0600`; profili pprof scritti `0600` | Le credenziali sono in chiaro nel file di configurazione dell'utente (come `mc`, `aws`) |
| E | Comandi eseguiti dalla CLI | Solo il programma che l'utente passa esplicitamente (`find --exec`, `internal/cli/commands/find.go`, `deps.go`) | — |
| T | Numeri fuori range nei documenti JSON | I valori `int32` dei documenti (giorni del lifecycle, `MaxAgeSeconds`) sono saturati invece che troncati (`s3sdk_config.go` `intPtr32`): `2^32+1` giorni non diventano 1 giorno | — |

## 10. Scansioni automatiche (`make security`, job CI `security`)

`make security` esegue, con versioni fissate nel Makefile e nel workflow:

- `govulncheck@v1.1.4 ./...`: vulnerabilità note **raggiunte dal codice** (moduli e libreria standard della
  toolchain di `go.mod`);
- `gosec@v2.29.0 -exclude-dir=web ./...`: pattern di codice insicuro; ogni falso positivo porta accanto un
  `// #nosec Gxxx -- motivo` (gosec ignora `//nolint`);
- `npm audit --omit=dev --audit-level=moderate` in `web/`: dipendenze di produzione della dashboard.

### 10.1 govulncheck

Aggiornamenti applicati in M9 (compatibili con `go 1.24`): `github.com/go-chi/chi/v5` v5.2.2 (open redirect GO-2025-3770),
`github.com/rabbitmq/amqp091-go` v1.13.0 (otto vulnerabilità del client AMQP: DoS, OOM, overflow, TLS senza versione
minima, credenziali esposte), `github.com/aws/aws-sdk-go-v2/service/s3` v1.97.3 e
`aws/protocol/eventstream` v1.7.8 (panic del decoder EventStream GO-2026-5764).

Mitigazioni nel codice, introdotte in M9 per vulnerabilità i cui fix richiedevano una toolchain più recente:

| Vulnerabilità | Raggiungibilità | Mitigazione |
|---------------|-----------------|-------------|
| GO-2026-5970 (`x/text/unicode/norm`, loop infinito su UTF-8 invalido) | `match.toNFC` normalizza chiavi controllate dai client | Le sequenze invalide non arrivano più a `norm`: solo i tratti validi sono normalizzati (`internal/service/match/normalize.go`, test `TestNormalizeInvalidUTF8`, fuzz con watchdog `FuzzNormalize`) |
| GO-2026-5004 (pgx, SQL injection con letterali dollar-quoted nel protocollo semplice) | `postgresConn.Exec` | Protocollo esteso forzato (`event/dialers.go`); gli statement sono statici |
| GO-2026-6629 (`x/text/secure/precis`, panic) | solo via `pgx.Connect` (SCRAM sulla password configurata) | Input dell'amministratore, non dei client |

**Toolchain aggiornata con la v1.0** (integrazione finale): le vulnerabilità della libreria standard di Go 1.24
(`net/http`, `crypto/tls`, `crypto/x509`, `encoding/xml`, `encoding/asn1`, `net/url`, `net/textproto`, `os`) e quelle
di `golang.org/x/net` (HTTP/2: GO-2026-6617, -6612, -6611, -6610, -6603, -4918, -4559; idna GO-2026-5026),
`golang.org/x/text`, pgx, `go.opentelemetry.io/otel` e `google.golang.org/grpc` sono chiuse passando a:

```sh
go get go@1.26.9 golang.org/x/net@v0.60.0 golang.org/x/text@v0.42.0 github.com/jackc/pgx/v5@v5.9.2 \
  go.opentelemetry.io/otel@v1.45.0 go.opentelemetry.io/otel/sdk@v1.45.0 \
  go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.45.0 \
  go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.45.0 \
  google.golang.org/grpc@v1.83.2 github.com/klauspost/compress@v1.18.7 github.com/go-chi/chi/v5@v5.3.0
go mod tidy
```

(`x/net` v0.60.0 richiede `x/text` v0.42.0; grpc v1.83.2, `klauspost/compress` v1.18.7 e chi v5.3.0 chiudono
GO-2026-6443, GO-2026-5841 e GO-2026-5774/5775/5777/4316, presenti nei moduli ma non raggiunti.) La CI usa
golangci-lint v2.14.0 (la 2.5.0 è compilata con Go 1.25.1 e rifiuta i moduli `go 1.26`) e i Dockerfile
`GO_VERSION=1.26.9`. Con `x/net` v0.60.0 il trasporto HTTP/2 di `x/net/http2` e `h2c` sono deprecati: il client RPC
internodo usa l'HTTP/2 di `net/http` (`http.Transport` con `Protocols` HTTP/2 o HTTP/2 in chiaro e `HTTP2Config`,
`internal/transport/rpc/conn.go`), come già faceva il server (`http.Server.Protocols`).

`govulncheck@v1.1.4 ./...` (eseguito con la toolchain di `go.mod`, `GOTOOLCHAIN=go1.26.9` in locale): **nessuna
vulnerabilità raggiunta**; resta solo GO-2026-5932 in `golang.org/x/crypto` (nessuna versione corretta, non raggiunta).
Le mitigazioni della tabella sopra restano nel codice come difesa in profondità.

### 10.2 gosec

150 segnalazioni iniziali (tutte High/Medium tranne una). Correzioni:

| Regola | Correzione |
|--------|-----------|
| G115 | Peso delle voci della cache saturato a `MaxUint32` (`cache/store.go`); numero di parte limitato a 32 bit prima della derivazione della chiave (`crypto/key.go`); valori `int32` della CLI saturati (`cli/s3sdk_config.go`) |
| G122 | Rimozioni durante la scansione della cache tramite `os.Root` (`cache/disk.go`) |
| G302/G306 | Voci della cache e coda dei webhook di purge `0600`; export della CLI `0600` (`access.go`, `cache.go`, `dist.go`) |

I falsi positivi (138 righe `#nosec`, ciascuna con il motivo) sono: G101 su nomi di header, parametri, variabili
d'ambiente e azioni IAM (21); G115 su conversioni con limiti verificati o reinterpretazioni volute dei bit (54); G304
su file scelti dall'operatore o dall'utente della CLI, o su percorsi già risolti e verificati dallo storage (26);
G401/G501/G505 su MD5/SHA1 imposti dal protocollo S3 (ETag, Content-MD5, MD5 della chiave SSE-C,
`x-amz-checksum-sha1`), mai usati come primitive di sicurezza (18); G404 sul jitter dei retry (3); G407 sul nonce
zero della sigillatura, sicuro perché la KEK è derivata da un sale casuale per ogni sigillatura e cifra un solo
messaggio; G402 sul `tls_skip_verify` opt-in; G124 sui cookie (`Secure` segue la connessione); G117, G118, G204,
G301, G602, G705 motivati puntualmente. Dopo le correzioni gosec riporta solo un G117 nel codice del servizio di
audit di M8 (`internal/service/audit/audit.go`, campo `AccessKey`: l'identificativo pubblico della chiave, lo stesso
falso positivo annotato in `share.go`), da annotare con il proprio `#nosec`.

### 10.3 npm audit

`npm audit --omit=dev` in `web/`: 0 vulnerabilità.

## 11. Fuzzing (`make fuzz`, job CI notturno `fuzz-nightly`)

`make fuzz` trova ogni `func FuzzXxx(f *testing.F)` ed esegue ciascun target per `FUZZTIME` (default 60s,
`FUZZ=regexp` per selezionarli); un crash lascia l'input in `testdata/fuzz/<Target>/` del pacchetto, da committare
con la correzione come test di regressione.

| Superficie | Target |
|-----------|--------|
| Parser XML S3 (tutti i corpi), policy JSON di bucket | `test/fuzz` `FuzzS3Bodies` (end-to-end su un server in-process, firmati SigV4 come root: versioning, tagging, CORS, lifecycle, notifiche, object lock, retention, legal hold, cifratura, delete multiplo, complete multipart, create bucket, policy) |
| Admin API JSON | `test/fuzz` `FuzzAdminJSON` (45 operazioni principali: bucket, IAM, policy, regole di accesso, distribuzioni, key group, certificati, cache, share, presign) |
| SigV4 (header, query, chunked, POST policy) | `internal/service/auth` `FuzzParseAuthorization`, `FuzzParseQuery`, `FuzzVerifyAuthorization`, `FuzzVerifyPresignedQuery`, `FuzzChunkedBody`, `FuzzPostPolicyDocument` |
| Policy JSON | `internal/service/policy` `FuzzParse`, `FuzzConditionValues` |
| xl.meta | `internal/service/storage` `FuzzDecodeXLMeta`; bitrot `internal/service/erasure` `FuzzBitrotReader` |
| Endpoint ed ellissi | `internal/domain` `FuzzParseEndpoint`, `internal/service/topology` `FuzzExpandArg` |
| Match (glob/regex/normalizzazione) | `internal/service/match` `FuzzNormalize`, `FuzzCompileAndMatch` |
| Firme e percorsi delle distribuzioni | `internal/service/distribution` `FuzzVerifySignedRequest`, `FuzzWebsitePath`, `FuzzResolve` |
| Lifecycle, CORS, notifiche XML | `internal/service/ilm` `FuzzParseLifecycle`, `internal/service/bucket` `FuzzParseCORS`, `internal/service/event` `FuzzParseBucketConfig` |
| Range header | `internal/domain` `FuzzParseRange` |
| Target della CLI | `internal/cli` `FuzzTargetParser` |

Gli oracoli non si limitano all'assenza di panic: errori solo di tipo S3, idempotenza della normalizzazione,
round-trip della configurazione delle notifiche e degli endpoint, coerenza di `Match` delle regole CORS, nessuna
risposta 5xx (tranne 501) dal server.

Esecuzione di M9 (60 s per target, 2 worker, 4 vCPU): 24 target, da 80 mila (end-to-end) a 1,4 milioni di
esecuzioni ciascuno. Un solo difetto trovato: `ParseEndpoint("0 /")` accettava la directory `"0 "`, che riletta
dopo il trim degli argomenti diventava `"0"` (un altro drive); ora un percorso che inizia o finisce con spazi è
rifiutato (`internal/domain/endpoint.go`, corpus in `internal/domain/testdata/fuzz/FuzzParseEndpoint/`).

## 12. Rischi residui consolidati e raccomandazioni

1. **Toolchain**: con la v1.0 Go 1.26.9 e i moduli aggiornati (§10.1), `govulncheck` senza vulnerabilità raggiunte;
   ogni release deve ripetere `make security` e aggiornare la toolchain alle patch di Go.
2. **Nessun rate limit per client sull'API S3 e sui domini pubblici**: mettere Nexo dietro un reverse proxy con
   limiti di richieste e connessioni (configurazioni in `deploy/proxy`).
3. **TLS opzionale**: senza TLS cookie della console, firme replayabili nella finestra di skew e traffico internode
   sono esposti. In produzione: `--certs-dir` su tutti i listener e `--tls-internode-verify`.
4. **Credenziali root = chiave del cluster**: chi le conosce firma token internode e sessioni; ruotarle con la
   procedura di docs/07 e non riusarle tra deployment (il deployment id separa comunque le chiavi derivate).
5. **Chiaro nel tier L2 della cache** per oggetti cifrati serviti dalle distribuzioni (§6).
6. **Master key del KMS builtin nell'ambiente del processo** (§8): in produzione KES con certificato client.
7. **CA dei peer per i client RPC lette all'avvio** (§4): una nuova CA richiede un riavvio coordinato.
8. **Profili pprof**: contengono indirizzi, stack e frammenti di memoria; concedere `admin:Profiling` solo a chi
   amministra i nodi.
9. **Client lenti o fermi**: ogni scrittura di una risposta deve avanzare entro `NEXO_SEND_TIMEOUT` (20 s), poi
   la risposta viene interrotta, e una connessione HTTP/2 che non accetta byte per lo stesso tempo viene chiusa
   con tutti i suoi stream. Su HTTP/2 le scritture avanzano a pezzi dimensionati sugli stream della connessione,
   così un client che legge con regolarità almeno circa 40 KB/s per connessione non viene interrotto, qualunque
   sia il numero di stream; più lento di così (o sotto circa 1,6 KB/s su HTTP/1) può esserlo. Fino ad allora un
   client non autenticato che non legge tiene un handler, il corpo della richiesta già ricevuto (al più la finestra
   HTTP/2 della connessione: 1 MiB, 16 MiB sul listener S3 di un cluster) e, per le letture di più di 8 parti, il
   lock di lettura dell'oggetto. Gli stream delle chiamate RPC autenticate (`httpx.LiftSendBound`) seguono il
   ritmo del client S3 a valle e non hanno il limite per stream, solo quello della connessione (un nodo legge
   sempre il proprio socket). Il numero di connessioni per client va limitato dal reverse proxy (punto 2).
