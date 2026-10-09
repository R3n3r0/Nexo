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
- Certificati da `~/.nexo/certs/` (`public.crt`, `private.key`, sottocartelle per SNI, `CAs/` per trust).
- Hot reload su cambio file (fsnotify) o SIGHUP.
- Opzione `--tls-internode-verify` per mTLS tra nodi.

## 7.4 Osservabilità
- **Metriche**: `nexo_s3_requests_total{api,status}`, `nexo_s3_request_duration_seconds`, `nexo_drive_*`
  (online, used, free, errors, latency), `nexo_heal_*`, `nexo_scanner_*`, `nexo_ilm_*`, `nexo_cluster_nodes_{online,offline}`.
- **Log**: JSON `slog` con `request_id`, `node`, `api`, `bucket`, `object`, `duration`, `error`.
- **Audit**: record per ogni richiesta autenticata con identità, azione, risorsa, IP, esito, latenza.
- **Tracing**: OpenTelemetry (OTLP) con propagazione del contesto sulle RPC internode.
- **Alert d'esempio** (Prometheus rules in `deploy/`): disco offline, quorum a rischio, healing bloccato, capacità > 85%, tasso 5xx.

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
| `api.cors_allow_origin` (config, `nexoctl admin config set`, applicata a caldo) | `*` | Origini CORS ammesse per tutto il deployment (separate da virgola, `*` o vuoto: ogni origine): vale per le richieste di servizio e per i bucket **senza** configurazione CORS, la cui preflight risponde 200 invece del 403 di S3 (vedi [02 §2.4](02-funzionalita.md)); la configurazione CORS di un bucket la sostituisce per quel bucket |
| `NEXO_BROWSER` | `on` | Abilita/disabilita la console |
| `NEXO_CONSOLE_SESSION_DURATION` | `12h` | Durata sessione console |
| `NEXO_PROMETHEUS_AUTH_TYPE` | `jwt` | `jwt` o `public`. Con `jwt` le rotte `/nexo/metrics/v1/*` richiedono `Authorization: Bearer <JWT>`: HMAC (HS256/384/512) firmato con la secret key del `sub` (access key), `iss=prometheus`, `exp` obbligatorio (formato di `mc admin prometheus generate`); l'identità deve avere `admin:Prometheus` (root o policy `diagnostics`). Il token non è mai una credenziale S3 |
| `--certs-dir` | `~/.nexo/certs` | Certificati TLS |
