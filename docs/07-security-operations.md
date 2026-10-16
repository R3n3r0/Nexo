# 7. Sicurezza e operatività

## 7.1 Threat model (sintesi STRIDE)

| Minaccia | Superficie | Mitigazione |
|----------|-----------|-------------|
| Spoofing | API S3, Admin API, internode | SigV4 con confronto costante, JWT internode a breve scadenza, mTLS opzionale tra nodi |
| Tampering | Dati su disco, traffico | Bitrot hash per blocco, TLS 1.2+ (default 1.3), SSE |
| Repudiation | Operazioni amministrative | Audit log immutabile verso target esterno |
| Information disclosure | Console, errori, log | Nessun segreto nei log, messaggi d'errore generici verso anonimi, segreti IAM cifrati a riposo |
| Denial of service | Upload, listing, login | Limiti di dimensione header/body, timeout, rate limit login, limiti di concorrenza per API |
| Elevation of privilege | Policy engine, console | Deny-by-default, autorizzazione sempre server-side, test negativi sistematici |

L'analisi completa per superficie (S3, Admin API e console, RPC internode, distribuzioni, cache, eventi, KMS, CLI),
con le mitigazioni nel codice, i rischi residui, le scansioni (`make security`: govulncheck, gosec, npm audit) e i
target di fuzzing (`make fuzz`), è in [docs/security/threat-model.md](security/threat-model.md).

## 7.2 Gestione dei segreti
- Credenziali root solo da env/file (`NEXO_ROOT_USER`, `NEXO_ROOT_PASSWORD` o `_FILE`), mai salvate in chiaro.
- IAM e config cifrati in `.nexo.sys` con chiave derivata (Argon2id/HKDF) o fornita dal KMS.
- Secret key degli utenti mostrata una sola volta in console/CLI.
- File config della CLI con permessi 0600 e avviso se più permissivi.
- Nessuna chiave nei log: la chiave SSE-C (`domain.SSECustomerKey`), le richieste SSE (`domain.SSERequest`), le
  master key del KMS (`kms.MasterKey`), le chiavi dati del KMS (`kms.DEK`) e le chiavi di oggetto in chiaro
  (`crypto.ObjectKey`) si stampano (`%v`, `%+v`, `%#v`), si registrano (`slog`) e si codificano in JSON solo in forma
  redatta (MD5 della chiave SSE-C, nome e fingerprint della master key, id della chiave e dimensione della forma
  sigillata di una DEK, tipo di cifratura e cifrario di una chiave di oggetto). Le credenziali nell'URL di un
  webhook (user info, query: `?token=...`), e in ogni valore di configurazione dato come URL (es. l'indirizzo NATS o
  Redis `nats://user:pass@host`, `redis://:pass@host`), sono mascherate in `GET /config` come i valori sensibili (il
  valore mascherato rimandato indietro conserva quello salvato) e tolte dagli errori di consegna, dai log e dallo stato
  dei target; l'endpoint KES (`NEXO_KMS_ENDPOINT`) compare negli errori di avvio senza user info né query.
- I webhook **non seguono i redirect**: una risposta 3xx è una consegna fallita (`endpoint returned 3xx`). Il
  destinatario di un webhook (spesso una terza parte) non può così rimbalzare la POST di Nexo, con il corpo e
  l'`auth_token` del target, verso host interni che nessun amministratore ha configurato (`127.0.0.1`,
  `169.254.169.254`: SSRF); un client HTTP iniettato nella factory dei target (`event.FactoryDeps.HTTP`) viene usato
  con la stessa politica.
- `queue_dir` dei target di notifica è un percorso **relativo** alla radice delle code
  (`<primo disco locale>/.nexo.sys/events/queue_dir/<queue_dir>`): percorsi assoluti e `..` sono rifiutati, perché la
  coda consuma e rimuove i file della sua directory.

### KMS e cifratura lato server (SSE)
- **KMS integrato** (`NEXO_KMS_SECRET_KEY="<nome>:<base64 di 32 byte>"`): la master key sigilla le chiavi dati di
  SSE-S3 e SSE-KMS (le chiavi create con `nexoctl admin kms key create` derivano da essa; il registro dei nomi sta nei
  documenti di sistema). Nei dati su disco non compare mai il contenuto in chiaro; i checksum (`x-amz-checksum-*`)
  sono sigillati con una chiave derivata da quella dell'oggetto. Per **SSE-C e SSE-KMS** anche l'ETag è opaco: i
  dischi non contengono alcun digest del contenuto in chiaro. Per **SSE-S3**, come in S3, l'ETag (di un PUT e di ogni
  parte) è l'MD5 del contenuto in chiaro, restituito ai client e quindi conservato in chiaro nei metadati della
  versione: SSE-S3 protegge dal furto dei dischi ma l'MD5 permette di confermare un'ipotesi sul contenuto (per un
  oggetto di pochi byte anche di ricavarlo); per dati che non devono rivelare alcun digest usare SSE-KMS o SSE-C.
- **KMS esterno (KES)**: `NEXO_KMS_ENDPOINT` accetta solo URL `https://` (le chiavi dati viaggiano in chiaro nel
  corpo delle risposte di KES) e il client non segue i redirect; KES autentica i client con **mTLS**
  (`NEXO_KMS_CLIENT_CERT`/`NEXO_KMS_CLIENT_KEY`, CA in `NEXO_KMS_CAPATH`): senza certificato client il server avvisa
  all'avvio.
- **SSE-C** richiede TLS, come S3: su HTTP in chiaro le richieste con la chiave sono rifiutate (`400 InvalidRequest`).
  `NEXO_SSEC_ALLOW_INSECURE=on` le accetta: **solo per test e sviluppo**. Con `nexoctl` la chiave va data preferibilmente
  con `NEXOCTL_ENC_C` (stessa sintassi di `--enc-c`, voci separate da virgola) o con un file di chiavi
  (`--enc-c-file`, una voce `ALIAS/BUCKET[/PREFIX]=BASE64KEY` per riga, permessi 0600): negli argomenti (`--enc-c`) è
  leggibile dagli altri utenti locali (`/proc/<pid>/cmdline`) e resta nella history della shell.
- **Cluster**: i nodi confrontano all'avvio l'impronta della configurazione KMS (master key corrente e precedenti, o
  endpoint KES e chiave di default, mai i segreti): un nodo con un KMS diverso dalla maggioranza non parte
  (`ErrKMSMismatch`), perché gli oggetti cifrati da un nodo sarebbero illeggibili sugli altri.
- **Rotazione della master key** (KMS integrato):
  1. avviare tutti i nodi con la nuova chiave in `NEXO_KMS_SECRET_KEY` e la precedente in
     `NEXO_KMS_SECRET_KEY_PREVIOUS` (più chiavi separate da virgola): le chiavi dati sigillate dalla precedente restano
     leggibili;
  2. ri-sigillare le chiavi dati di tutti gli oggetti SSE-S3/SSE-KMS con la chiave corrente:
     `nexoctl admin kms reseal ALIAS [--bucket B]` (Admin API `POST /kms/reseal?bucket=`, azione
     `admin:KMSCreateKey`); i dati non vengono riscritti. Sono ri-sigillate anche le chiavi degli upload multipart in
     corso (la loro descrizione contiene la chiave sigillata dell'oggetto: senza, dopo il passo 3 non accetterebbero più
     parti né il completamento). Il comando riporta versioni e upload esaminati, chiavi ri-sigillate e fallite (esce con
     errore se una chiave fallisce: va ripetuto);
  3. quando il comando non riporta errori su tutti i bucket, rimuovere `NEXO_KMS_SECRET_KEY_PREVIOUS` e riavviare.
  Con KES la rotazione delle chiavi avviene sul server KES; `reseal` porta le chiavi dati sulla versione corrente.

## 7.3 TLS
- Certificati da `--certs-dir` (`NEXO_CERTS_DIR`; senza, i listener servono HTTP in chiaro), servizio
  `certs.Keystore` (`internal/service/certs/keystore.go`):
  - `public.crt` + `private.key`: certificato di default (obbligatorio; catena PEM foglia + intermedi, chiave PKCS #8,
    PKCS #1 o SEC 1, al massimo 1 MiB per file; una chiave leggibile da altri utenti produce un avviso);
  - `<nome>/public.crt` + `<nome>/private.key`: certificati aggiuntivi scelti per **SNI** sui nomi DNS della foglia
    (esatto, poi wildcard di un livello; a parità vince il valido che scade per ultimo); le cartelle nascoste (`..data`
    dei secret Kubernetes) e quelle senza coppia sono ignorate;
  - `CAs/*`: CA fidate per i peer. I certificati **client** dei peer (`--tls-internode-verify`) sono verificati solo
    con le CA di `CAs/` e con il certificato di default stesso (un cluster che condivide un unico certificato),
    **mai** con gli intermedi della catena di `public.crt`: una CA che emette il certificato del server non diventa
    fidata per ogni certificato che ha emesso. Il certificato client deve inoltre nominare (SAN DNS o IP) l'host di
    un endpoint dei nodi. I certificati **server** dei peer sono verificati con le radici di sistema, `CAs/` e la
    catena di `public.crt`.
- **Hot reload senza restart né interruzione**: la directory (e quelle dei bersagli dei link simbolici, come `live/` e
  `archive/` di certbot) è osservata con fsnotify e un debounce di 500 ms, con una scansione di sicurezza ogni minuto;
  `SIGHUP` (`systemctl reload nexo`) ricarica subito anche i certificati delle distribuzioni. Ogni handshake legge il
  materiale corrente: le connessioni già aperte (HTTP/1.1 e HTTP/2) e le risposte in corso proseguono con il
  certificato con cui sono nate. Una ricarica che fallisce (chiave che non corrisponde, file scritto a metà) mantiene i
  certificati precedenti e viene registrata (`nexo_tls_server_certificate_reloads_total{result}`); la scadenza è
  esposta in `nexo_tls_server_certificate_expiry_seconds{certificate}` con avviso nei log a 21 giorni.
  `POST /service/restart` non usa più `SIGHUP`: il riavvio è una richiesta interna al processo.
- **SNI multipli** sui listener S3 e console e, come fallback, sul listener TLS delle distribuzioni: catena unica
  `GetCertificate` = certificati delle distribuzioni (solo per i loro alias) → sottocartelle SNI → default.
- **Politica**: TLS 1.2 minimo (`--tls-min-version` / `NEXO_TLS_MIN_VERSION`: `1.2` o `1.3`), TLS 1.3 negoziato con ogni
  client che lo supporta; in TLS 1.2 solo suite ECDHE con AEAD (AES-GCM, ChaCha20-Poly1305); ALPN `h2`, `http/1.1`.
- Opzione `--tls-internode-verify` per mTLS tra nodi: il nodo presenta il certificato corrente del keystore (anche
  dopo una rotazione) e verifica quello dei peer con le CA correnti di `CAs/` (o il certificato condiviso): il
  certificato deve nominare uno degli host degli endpoint, così un certificato della stessa CA emesso per un altro
  servizio non passa. Con certificati di nodo emessi da una CA privata, la CA va messa in `CAs/`. La fiducia verso i server dei peer (radici del
  client internode) è fissata all'avvio: la sostituzione della **CA** (o di un certificato autofirmato usato come
  radice) richiede un riavvio a rotazione dei nodi; il rinnovo di una foglia firmata dalla stessa CA no.

## 7.4 Osservabilità
- **Metriche Prometheus** su `/nexo/metrics/v1/{node,cluster,bucket}` (porta S3, `NEXO_PROMETHEUS_AUTH_TYPE`):
  - `node`: le famiglie del nodo che risponde con la label `server`; `cluster`: le famiglie di **ogni** nodo
    (raccolte via RPC, label `server`; un nodo irraggiungibile è escluso dallo scrape) più le famiglie
    `nexo_cluster_*` calcolate dallo stato del deployment; `bucket`: le famiglie `nexo_bucket_*` sommate su tutti i
    nodi più l'utilizzo per bucket del scanner.
  - S3: `nexo_s3_requests_total{api,status}`, `nexo_s3_requests_{4xx,5xx}_errors_total{api}`,
    `nexo_s3_request_duration_seconds{api}`, `nexo_s3_requests_inflight`, `nexo_s3_traffic_{received,sent}_bytes_total`;
    bucket: `nexo_bucket_requests_total{bucket,api,status_class}`, `nexo_bucket_traffic_{received,sent}_bytes_total{bucket}`,
    `nexo_bucket_usage_total_bytes`, `nexo_bucket_{objects,versions,delete_markers}`.
  - Drive: `nexo_drive_{online,healing,total_bytes,used_bytes,free_bytes,used_inodes,free_inodes}{drive,pool,set}`,
    `nexo_drive_operations_total{drive,op}`, `nexo_drive_operation_seconds_total{drive,op}` (latenza media per
    operazione), `nexo_drive_errors_total{drive,type}` (`offline`, `faulty`, `corrupt`, `full`, `timeout`, `format`, `other`).
  - Cluster: `nexo_cluster_nodes_{online,offline}`, `nexo_cluster_drives_{online,offline,healing,total}`,
    `nexo_cluster_sets_{healthy,degraded,read_only,unavailable}{pool}`, `nexo_cluster_{read,write}_quorum{pool}`,
    `nexo_cluster_capacity_{raw,usable}_*_bytes`, `nexo_cluster_{buckets,objects,usage_bytes}`.
  - Healing e scanner: `nexo_mrf_queue_{pending,leased}`, `nexo_mrf_queue_{added,merged,dropped,healed,retried}_total`,
    `nexo_heal_jobs{state}`, `nexo_heal_running_objects_*`, `nexo_heal_drive_*{drive}` (drive in ripopolamento),
    `nexo_scanner_{active,scanning,cycles_total}`, `nexo_scanner_last_cycle_*`, `nexo_ilm_actions_total{action}`,
    `nexo_ilm_{uploads_aborted,errors,versions_evaluated}_total`.
  - Notifiche: `nexo_events_pending{target}`, `nexo_events_target_online{target}`, `nexo_events_{sent,failed}_total{target}`,
    `nexo_events_send_duration_seconds{target}`, `nexo_events_{published,dropped}_total`.
  - Internodo, lock, KMS: `nexo_rpc_{requests,errors}_total{peer,method}`, `nexo_rpc_request_duration_seconds{peer,method}`,
    `nexo_rpc_peer_online{peer}`, `nexo_lock_acquire_duration_seconds{type}`, `nexo_lock_acquire_{timeouts,errors}_total{type}`,
    `nexo_kms_online`, `nexo_kms_requests_total{op}`, `nexo_kms_errors_total{op,reason}`, `nexo_kms_request_duration_seconds{op}`.
  - Cache e distribuzioni: `nexo_cache_*{distribution,host,...}`, `nexo_distribution_*{distribution,...}`; processo:
    `nexo_node_uptime_seconds`, `nexo_node_info{version,commit}`, `go_*`, `process_*`.
  - Cardinalità limitata: nessuna chiave d'oggetto in label; un bucket etichetta una serie solo se la richiesta ne
    prova l'esistenza (risposta < 400, errore del server o richiesta autenticata non 404) e al massimo 1000 bucket per
    nodo (oltre: `_other`); i metodi RPC sono i percorsi fissi del protocollo.
- **Serie della console** (D-MON-01): ring buffer in memoria per nodo (un punto ogni 10 s per 24 h) delle serie chiave
  (`GET /api/v1/metrics` le elenca: richieste, errori, latenza media e p99, traffico, drive, healing, notifiche, RPC,
  CPU, memoria); `GET /api/v1/metrics/query?metric=&range=&step=&end=&node=` restituisce i punti di ogni nodo e
  l'aggregato del cluster (somma, media o massimo secondo la serie). Richiede `admin:Prometheus` **o** `admin:ServerInfo`.
- **Trace live** (S-OPS-07, D-MON-02): `GET /api/v1/trace` (Server-Sent Events, `admin:ServerTrace`) pubblica una
  richiesta S3, console, sito o RPC per evento (ora, nodo, API, metodo, percorso, query **redatta** — firme,
  credenziali, token, password, SSE-C, URL firmati delle distribuzioni e i nomi usuali delle credenziali applicative
  dei siti serviti (`apikey`, `api_key`, `api-key`, `x-api-key`, `auth`, `sig`, `jwt`, `session`, `sid`); gli
  altri parametri applicativi restano in chiaro —, stato, durata, byte, request id, IP, utente, errore) di tutti i nodi, con filtri `node`,
  `type`, `api`, `bucket`, `status` (codici o classi `5xx`), `errors`, `threshold`. Header e body non sono mai
  pubblicati. Senza sottoscrittori il costo è nullo; un client lento perde eventi (evento `dropped` con il contatore,
  `nexo_trace_dropped_total`) senza mai rallentare le richieste; heartbeat ogni 15 s.
- **Log live** (D-MON-02): `GET /api/v1/logs` (SSE, `admin:ConsoleLog`, filtri `node`, `level`, `last` fino a 10000
  voci per nodo ri-inviate all'apertura). I log restano JSON (o testo) su stderr; livello e formato si cambiano a caldo
  con il sottosistema `logger` (`nexoctl admin config set logger level=debug format=text`; senza valore valgono
  `--log-level`/`--log-format`). Segreti (Authorization, firme, credenziali, token, password, cookie, chiavi SSE-C)
  redatti dai flussi.
- **Audit** (S-OPS-06, D-MON-03): un record per ogni richiesta S3 e console (identità, azione/API, risorsa, IP,
  esito, latenza, request id, nodo, byte), tranne le sonde di salute anonime (`/nexo/health/*` senza credenziali)
  e gli scrape anonimi riusciti delle metriche (`/nexo/metrics/*` senza identità né token, stato < 400; una
  richiesta con credenziali, firma o token, e uno scrape rifiutato sono sempre registrati). Il record è scritto
  nel log JSON (`msg=audit`), tenuto in memoria (ultimi 10000 per nodo, ricerca `GET /api/v1/audit` con
  `admin:ConsoleLog`, aggregata sui nodi) e consegnato ai target `audit_webhook[:<id>]` e `audit_kafka[:<id>]`
  (stesse chiavi dei target `notify_*` di pari tipo, coda persistente in `<primo drive>/.nexo.sys/audit`, applicati
  a caldo). Il webhook riceve un documento JSON per record; Kafka un
  messaggio con chiave `<nodo>/<request id>`.
- **Tracing distribuito** (T-8.5): OpenTelemetry OTLP opzionale (`NEXO_OTEL_EXPORTER_OTLP_ENDPOINT`, protocollo
  `http/protobuf` o `grpc`, campionamento `NEXO_OTEL_TRACES_SAMPLER_ARG` con decisione parent-based): uno span server
  per richiesta S3/console/sito/RPC che continua il `traceparent` W3C del chiamante, uno span client per ogni chiamata
  internodo che lo propaga. Attributi senza segreti (metodo, percorso senza query, stato, API, bucket, request id,
  client). Disattivato (default) il costo è nullo: i middleware sono l'handler stesso.
- **Alert e dashboard** in `deploy/docker/observability/` (validati da `config_test.go` contro le famiglie
  esposte): nodo/drive offline, set degradati, quorum di scrittura perso, KMS irraggiungibile, capacità > 85%, drive
  quasi pieni o senza inode, tasso 5xx, latenza p99, errori internodo, timeout dei lock, healing bloccato (coda MRF e
  drive), coda eventi e target non raggiungibili; dashboard Grafana `nexo-overview`.
- **Configurazione dinamica applicata** (`settings.Applied`): `logger.level`, `logger.format`, `audit_*`, `notify_*`,
  `api.root_access`, `api.cors_allow_origin`, `scanner.*`, `access.*`. `api.requests_max` e `api.requests_deadline`
  sono validati e salvati ma **non ancora applicati** (limite di concorrenza: hardening M9).

## 7.5 Deploy
| Target | Artefatto |
|--------|-----------|
| Bare metal / VM | Binari statici (linux/darwin/windows, amd64/arm64), unit systemd, file env |
| Container | Immagine distroless multi-arch firmata (cosign) + SBOM |
| Docker Compose | Esempi single-node e 4 nodi |
| Kubernetes | Helm chart (StatefulSet, headless service, PVC per disco, PodDisruptionBudget, ServiceMonitor) |

### Raccomandazioni operative
- Dischi dedicati formattati XFS, montati con `noatime`, niente RAID sotto Nexo.
- Stesso numero e dimensione di dischi per nodo all'interno di un pool.
- NTP sincronizzato su tutti i nodi.
- Load balancer davanti ai nodi per S3 (9000) e console (9001) con health check su `/nexo/health/ready`.

## 7.6 Configurazione principale

| Variabile / flag | Default | Descrizione |
|------------------|---------|-------------|
| `--address` / `NEXO_ADDRESS` | `:9000` | Porta API S3 |
| `--console-address` / `NEXO_CONSOLE_ADDRESS` | `:9001` | Porta console/Admin API |
| `NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD` | — (obbligatori, min 8 caratteri) | Credenziali root |
| `NEXO_REGION` | `us-east-1` | Regione S3 |
| `NEXO_DOMAIN` | — | Abilita virtual-host style |
| `NEXO_STORAGE_CLASS_STANDARD` | auto (`EC:2..4`) | Parità di default |
| `NEXO_KMS_SECRET_KEY` (o `_FILE`) | — | Master key del KMS integrato, `<nome>:<base64 di 32 byte>`; il nome è la chiave di default di SSE-S3 |
| `NEXO_KMS_SECRET_KEY_PREVIOUS` (o `_FILE`) | — | Master key sostituite (separate da virgola): solo per leggere gli oggetti che hanno sigillato, fino al `reseal` (§7.2) |
| `NEXO_KMS_ENDPOINT` | — | URL `https://` dei server KES, separati da virgola (failover nell'ordine); `http://` è rifiutato |
| `NEXO_KMS_KEY_NAME` | — | Chiave di default su KES (SSE-S3 e SSE-KMS senza key id) |
| `NEXO_KMS_CLIENT_CERT` / `NEXO_KMS_CLIENT_KEY` | — | Certificato e chiave client (PEM) per l'mTLS verso KES |
| `NEXO_KMS_CAPATH` | radici di sistema | CA (file o directory PEM) che firmano i certificati dei server KES |
| `NEXO_SSEC_ALLOW_INSECURE` | `off` | Accetta chiavi SSE-C su HTTP in chiaro: **solo test e sviluppo** |
| `api.cors_allow_origin` (config, `nexoctl admin config set`, applicata a caldo) | `*` | Origini CORS ammesse per tutto il deployment (separate da virgola, `*` o vuoto: ogni origine): vale per le richieste di servizio e per i bucket **senza** configurazione CORS, la cui preflight risponde 200 invece del 403 di S3 (vedi [02 §2.4](02-features.md)); la configurazione CORS di un bucket la sostituisce per quel bucket |
| `NEXO_PROMETHEUS_AUTH_TYPE` | `jwt` | `jwt` o `public`. Con `jwt` le rotte `/nexo/metrics/v1/*` richiedono `Authorization: Bearer <JWT>`: HMAC (HS256/384/512) firmato con la secret key del `sub` (access key), `iss=prometheus`, `exp` obbligatorio (formato di `mc admin prometheus generate`); l'identità deve avere `admin:Prometheus` (root o policy `diagnostics`). Il token non è mai una credenziale S3 |
| `--certs-dir` / `NEXO_CERTS_DIR` | — (TLS disattivato) | Certificati TLS (§7.3), ricaricati a caldo |
| `--tls-min-version` / `NEXO_TLS_MIN_VERSION` | `1.2` | Versione TLS minima accettata dai listener (`1.2` o `1.3`) |
| `NEXO_OTEL_EXPORTER_OTLP_ENDPOINT` | — (tracing disattivato) | URL `http(s)://` del collector OTLP: base URL per `http/protobuf` (`/v1/traces` aggiunto se manca il percorso), host:porta per `grpc`; valgono anche `OTEL_EXPORTER_OTLP_HEADERS`, `_TIMEOUT`, `_CERTIFICATE` |
| `NEXO_OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` | `http/protobuf` o `grpc` |
| `NEXO_OTEL_TRACES_SAMPLER_ARG` | `1` | Quota (0-1) delle tracce avviate dal nodo; una richiesta con `traceparent` campionato è sempre registrata |
| `logger.level` / `logger.format` (config, applicate a caldo) | `--log-level` / `--log-format` | Livello e formato dei log |
| `audit_webhook[:<id>]` / `audit_kafka[:<id>]` (config, applicate a caldo) | — | Target del log di audit (chiavi dei `notify_webhook`/`notify_kafka`) |

La console è sempre servita sul listener della console (`--console-address`, che serve anche l'Admin API):
non esiste una variabile per disattivarla, si limita l'esposizione della porta (firewall, proxy). La durata
di una sessione della console è fissa a 12 ore (non configurabile); il logout e la rotazione del secret la
revocano prima.

## 7.7 Diagnostica (S-OPS-11)
- **Profiling pprof** solo tramite Admin API (`POST /api/v1/profile`, `nexoctl admin profile`): nessun endpoint
  `/debug/pprof` è esposto sui listener. Richiede `admin:Profiling` (root e policy `diagnostics`; `consoleAdmin`
  tramite `admin:*`). `cpu`, `mutex` e `block` campionano per 1–120 s (default 10 s; il campionamento di mutex e
  block è attivo solo durante la richiesta), `heap` e `goroutine` sono istantanei; un profilo alla volta per nodo.
  Un profilo rivela nomi di funzioni e, per `goroutine`, gli stack: trattarlo come dato riservato.
- **Speed test** (`POST /api/v1/speedtest/{drive,object,net}`, `nexoctl admin speedtest`), `admin:Speedtest`:
  scrivono e poi leggono per al massimo 60 s per fase, file/oggetti ≤ 64 MiB, ≤ 1 GiB per target, concorrenza
  ≤ 64, un test alla volta per nodo; i file stanno nell'area temporanea dei dischi (`.nexo.sys/tmp/speedtest-*`,
  rimossi alla fine, altrimenti dal janitor), il test `object` usa un bucket temporaneo `nexo-speedtest-*`
  rimosso alla fine. Caricano il server: eseguirli fuori dagli orari di punta.
- In cluster entrambi agiscono sul nodo che risponde (il suo indirizzo console); il test `net` misura la rete verso
  ogni altro nodo attraverso l'RPC internode.

