# Changelog

Tutte le modifiche rilevanti di Nexo. Il formato segue [Keep a Changelog](https://keepachangelog.com/it/1.1.0/) e
le versioni seguono [Semantic Versioning](https://semver.org/lang/it/). I codici tra parentesi (`S-OBJ-06`,
`D-MON-01`, `C-ADM-08`) sono quelli del [catalogo delle funzionalità](docs/02-features.md); i test che li coprono
sono nella [matrice di tracciabilità](test/TRACEABILITY.md).

## [1.0.0] - da pubblicare

Prima release stabile: milestone M0–M9 della [roadmap](docs/05-roadmap.md). Le release precedenti non sono mai
state pubblicate: la base dei test di aggiornamento è il commit di M7 (`224bb31`, costruito come `v0.7.0`).

### Modalità di deployment e storage
- Un solo binario per tre modalità rilevate dagli argomenti di `nexo server` (S-STO-00): singolo nodo e singolo
  disco senza erasure coding (SNSD, S-STO-01), singolo nodo con più dischi (SNMD, S-STO-02) e cluster multi-nodo
  (S-STO-06), con `format.json` e ellissi negli endpoint (`/mnt/disk{1...4}`, `http://nodo{1...4}/mnt/d{1...4}`).
- Erasure coding Reed-Solomon con set da 2 a 16 dischi, parità per storage class `STANDARD` e
  `REDUCED_REDUNDANCY` (S-STO-03), protezione dal bitrot con HighwayHash verificata in lettura (S-STO-04).
- Healing: in lettura, coda MRF persistente, scanner in background con heal a campione, sostituzione dei dischi
  con ripresa dopo il riavvio, `admin heal` (S-STO-05, C-ADM-07, D-CLU-02).
- Cluster: RPC internodo su HTTP/2 con msgpack e JWT legati alla chiamata, lock distribuiti a quorum (dsync,
  S-STO-07), bootstrap coordinato con attesa del quorum, leader per i job singleton (scanner, ILM, ACME),
  invalidazioni IAM/bucket/config tra i nodi, espansione con nuovi pool e placement pesato (S-STO-08).
- Letture su istantanea: una GET rilascia il lock dopo aver aperto i dati, così PUT e DELETE della stessa chiave
  non aspettano il trasferimento.
- Export/import dello stato del deployment (IAM, configurazione, metadati dei bucket) per backup e migrazione tra
  modalità (S-OPS-15, C-ADM-13).

### API S3
- Bucket, oggetti, range e richieste condizionali, listing V1/V2, CopyObject con `REPLACE` e copy-source-range,
  DeleteObjects, multipart completo con UploadPartCopy, metadati utente (S-BKT-01..03, S-OBJ-01..07).
- Firma SigV4 su header, query, streaming `aws-chunked` con trailer e `UNSIGNED-PAYLOAD`; URL presigned con
  scadenza massima di 7 giorni, POST policy, share link gestiti e revocabili (S-IAM-02, S-OBJ-08, 08b, 08c).
- Versioning con delete marker e versione `null`, tagging, CORS, lifecycle (expiration, noncurrent, multipart
  incompleti), Object Lock governance/compliance con legal hold, quota, notifiche (webhook, Kafka, NATS, AMQP,
  Redis, PostgreSQL) con coda persistente, cifratura SSE-S3/SSE-KMS/SSE-C (DARE) con KMS integrato o KES e cifratura
  predefinita del bucket, checksum CRC32/CRC32C/CRC64NVME/SHA1/SHA256, GetObjectAttributes (S-BKT-04..11,
  S-OBJ-09..13, S-OBJ-15).
- Compatibilità misurata: Ceph `s3-tests` 451/451 test applicabili e MinIO `mint` 638 test senza fallimenti
  inattesi, in SNSD, SNMD e cluster; differenze volute rispetto ad Amazon S3 in [02 §2.4](docs/02-features.md).

### IAM, accessi e sicurezza
- Utente root da ambiente o file (`*_FILE`), utenti, gruppi, policy IAM in formato AWS con condizioni e variabili,
  policy predefinite (`readonly`, `readwrite`, `writeonly`, `consoleAdmin`, `diagnostics`), simulatore e permessi
  effettivi, service account con policy inline e scadenza, STS AssumeRole (S-IAM-01..07).
- Regole di accesso per bucket con pattern prefix, glob e regex RE2, condizioni (IP, referer, host, HTTPS,
  finestra temporale, header, gruppi), preset pubblici, block public access e tester (S-ACC-01..04).
- TLS da `--certs-dir` con SNI, ricarica a caldo (fsnotify e SIGHUP) senza interrompere le connessioni, TLS ≥ 1.2
  (`--tls-min-version`), mTLS opzionale tra i nodi (S-OPS-02).
- Sessioni della console in cookie `HttpOnly`/`SameSite=Strict`, CSRF, controllo dell'origine anche tra porte
  dello stesso host, throttling dei login; segreti IAM cifrati a riposo.
- Threat model STRIDE ([docs/security/threat-model.md](docs/security/threat-model.md)), `make security`
  (govulncheck, gosec, `npm audit`), 24 target di fuzzing (`make fuzz`).

### Domini, distribuzioni e cache
- Distribuzioni in stile CloudFront: più domini (anche wildcard) per bucket o prefix, modalità `website` (index,
  error page, SPA fallback, redirect) e `s3`, behavior per path, URL e cookie firmati con key group, proxy fidati,
  PROXY protocol, origin secret, restrizioni IP, certificati per dominio caricati o ACME HTTP-01/TLS-ALPN-01, metriche
  e access log per distribuzione (S-DST-01..06, S-DST-08).
- Cache integrata in memoria e su disco con policy per distribuzione, dominio e percorso, quote, invalidazione
  automatica alla scrittura, purge per path/glob/regex/tag/dominio, webhook verso CDN esterne, cache condivisa in
  cluster, stale-while-revalidate/if-error, request collapsing e range caching (S-DST-07, 07b, 07c).

### Osservabilità (M8)
- Metriche Prometheus su `/nexo/metrics/v1/{cluster,node,bucket}`: API con istogrammi di latenza, dischi, rete,
  lock, KMS, eventi, healing, scanner, ILM, cache e distribuzioni; dashboard Grafana, alert e configurazioni
  Prometheus in `deploy/docker/observability` (S-OPS-04).
- Serie storiche per la console (ring buffer di 24 h per nodo, `/api/v1/metrics/query`).
- Trace live e log live aggregati da tutti i nodi via SSE, con filtri e redazione dei segreti; log strutturati
  JSON o testo con livello e formato cambiati a caldo (S-OPS-05, S-OPS-07).
- Audit di ogni richiesta autenticata con ricerca e target webhook/Kafka (S-OPS-06, D-MON-03).
- OpenTelemetry OTLP (`http/protobuf`, `grpc`) con uno span per richiesta e per chiamata internodo e
  propagazione `traceparent`.
- Diagnostica: speed test di drive, object layer e rete, profili pprof protetti da `admin:Profiling` (S-OPS-11,
  anticipata da M10).

### Admin API, console e CLI
- Admin API REST (`/api/v1`, contratto in [api/openapi.yaml](api/openapi.yaml)) con autorizzazione per azione
  `admin:*`, errori `problem+json` e client Go `pkg/nexoadmin`.
- Console React incorporata nel binario: overview, bucket e impostazioni, object browser con upload multipart,
  download zip, anteprime, versioni, retention, utenti, gruppi, policy (editor Monaco e builder), access key,
  condivisioni, regole di accesso, distribuzioni, cache, cluster e healing, metriche, trace e log live, audit,
  configurazione; tema chiaro/scuro, italiano/inglese, nessuna violazione serious/critical di axe (D-*).
- `nexoctl`: alias, comandi sui dati (`ls`, `cp`, `mv`, `rm`, `mirror --watch`, `find`, `du`, `diff`, `tree`,
  `share`, `presign`, `anonymous`), impostazioni dei bucket, `access`, `dist`, `cache`, `admin` (utenti, gruppi,
  policy, access key, config, heal, info, trace, logs, audit, metrics, prometheus generate, speedtest, profile,
  cluster export/import), output `--json`, autocompletamento, `--debug` con segreti redatti (C-*).

### Packaging e deploy
- Binari statici per Linux, macOS e Windows (amd64/arm64), archivi, pacchetti deb/rpm con unit systemd e utente
  di sistema, cask Homebrew, SBOM SPDX (S-OPS-12, C-UX-00).
- Immagini Docker multi-arch `nexo` e `nexoctl` distroless firmate con cosign, `nexo healthcheck` con TLS,
  segreti `*_FILE`, Compose per singolo nodo, SNMD, cluster (anche TLS e più proxy), stack Swarm, Helm chart
  `standalone`/`distributed` (S-OPS-13).
- `nexo server --dry-run` e URL pubblico (`NEXO_SERVER_URL`) per i link dietro proxy (S-OPS-14).

### Documentazione
- Documentazione utente in [docs/user](docs/user/README.md) (quickstart, installazione, modalità, configurazione,
  guida operativa, sicurezza, distribuzioni, riferimenti) con sito MkDocs e riferimento della CLI generato da cobra.
- Benchmark `warp` e profiling in [docs/bench](docs/bench/README.md).
- Matrice di tracciabilità [test/TRACEABILITY.md](test/TRACEABILITY.md): tutte le 147 funzionalità P0/P1 hanno
  almeno un test; `make traceability-check` blocca la release altrimenti.

### Modificato
- Toolchain Go 1.26.9 (`go.mod`, Dockerfile, CI) e golangci-lint v2.14.0; aggiornati `x/net` v0.60.0, `x/text`
  v0.42.0, pgx v5.9.2, OpenTelemetry v1.45.0, grpc v1.83.2, `klauspost/compress` v1.18.7, chi v5.3.0: `govulncheck`
  senza vulnerabilità raggiunte.
- Go toolchain upgraded to 1.27.2 (`go.mod`, `golang:1.27.2-alpine` builder images in the Dockerfiles; CI reads
  `go.mod`); golangci-lint v2.14.0 (built with Go 1.27) unchanged, no module dependency changes required. mockery
  v2.53.4 is now pinned in a separate tools module (`tools/go.mod`, run with `go tool -modfile`) built with
  `x/tools` v0.51.0, which reads the Go 1.27 export data; the regenerated mocks only differ in import grouping.
  govulncheck v1.8.0 (was v1.1.4, whose SSA builder panics on Go 1.27 code) in `make security` and CI.
  With Go 1.27 `encoding/json` is backed by the v2 implementation: the line and column of a policy JSON syntax
  error are now taken from the offending byte (`SyntaxError.Offset` counts it), and the race detector caught a
  `sync.WaitGroup` Add/Wait race in the RPC connection close (fixed by ordering both under the connection mutex).
- Il client RPC internodo usa l'HTTP/2 di `net/http` invece di `x/net/http2` (deprecato).
- SIGHUP ricarica solo i certificati TLS; `POST /api/v1/service/restart` riavvia il processo in place.
- Nuove azioni di policy `admin:Profiling` e `admin:Speedtest`, incluse nella policy predefinita `diagnostics`.
- Priorità del catalogo: S-OPS-11, D-MON-03 e C-ADM-12, consegnate in anticipo, passano da P2 a P1.

### Corretto (revisione finale)
- **Alias takeover delle distribuzioni**: gli host del deployment (endpoint dei nodi, `localhost`, `--server-url`,
  `--browser-redirect-url`, `--domain`) sono riservati; il prefisso `/nexo/` (salute, metriche, RPC) non passa mai
  dal routing per Host; l'RPC internodo salta solo i virtual host di `--domain`; un alias sotto il wildcard di
  un'altra distribuzione richiede i permessi su di essa; il listener S3 serve le distribuzioni solo con
  `--s3-distributions` (`auto`/`on`/`off`) e `--no-distributions` le disattiva per il ripristino (S-DST-01).
- **Letture su istantanea con ridondanza**: una GET che ha rilasciato il lock tiene aperti gli shard di tutti i
  dischi online (handle remoti via RPC), così bitrot, dischi o nodi persi durante il trasferimento non la
  troncano anche se una PUT o DELETE concorrente ha già eliminato i dati, anche se dura più di un'ora (il nodo che
  legge rinnova i suoi handle); il conteggio delle parti è corretto. Aprire e chiudere un handle costa lo stesso
  qualunque sia il numero di handle aperti sul nodo.
- **Client fermi su HTTP/2**: una connessione HTTP/2 che non accetta byte per `NEXO_SEND_TIMEOUT` viene chiusa con
  tutti i suoi stream su ogni listener; prima un client anonimo che annunciava finestre grandi e smetteva di
  svuotare il socket teneva a tempo indeterminato handler, corpi bufferizzati e il lock di lettura degli oggetti
  di più di 8 parti. Su HTTP/2 il limite di stallo misura l'avanzamento di ogni stream a pezzi dimensionati sugli
  stream della connessione: un browser che carica molte risorse in parallelo su una rete lenta non viene più
  interrotto.
- `/nexo/health/cluster` riporta nodi, dischi, quorum di scrittura e manutenzione reali (S-OPS-03);
  `admin info` riporta oggetti e spazio usato dallo scanner.
- License: Nexo is released under the GNU AGPL v3.0 only (`AGPL-3.0-only`, `LICENSE`) with additional attribution
  terms under AGPL section 7(b) (`NOTICE`), and is also available under commercial licenses
  (`COMMERCIAL-LICENSE.md`); contributions require the sign-off described in `CONTRIBUTING.md`. `LICENSE` ships in
  archives, deb/rpm packages, OCI images and the Helm chart (the chart package is checked by `make helm-lint`);
  `NOTICE` and `COMMERCIAL-LICENSE.md` ship in archives and `NOTICE` in the packages. The console shows the license,
  the "Powered by Nexo" attribution and a link to the source code (AGPL section 13), and `nexo --version` and
  `nexoctl --version` print the license.
- Audit, metriche e trace riportano bucket e oggetto risolti come l'API S3, dai domini personalizzati e dai
  virtual host scritti con il punto finale (`bucket.s3.example.com.`); l'audit non registra le sonde di salute
  anonime né gli scrape anonimi riusciti delle metriche, ma registra sempre le richieste con credenziali (firma,
  token: anche un tentativo fallito su `/nexo/health/*`) e gli scrape rifiutati.
- Log di accesso JSON delle distribuzioni: l'inizio della richiesta è `event_time`, non una seconda chiave `time`
  (rifiutata da Elasticsearch).
- Bootstrap: il nodo che parte dopo i peer di un deployment nuovo registra a INFO l'attesa dei suoi dischi vuoti;
  il WARN `empty local drives in an existing deployment` resta per i dischi sostituiti di un set con dati.
- mTLS internodo: i client accettati sono firmati dalle CA di `CAs/` e hanno un SAN tra gli host degli endpoint.
- Un solo predicato di redazione (firme, credenziali, token, password, policy e chiavi) per log, log stream,
  audit, access log e trace.
- `POST /api/v1/profile` invece di GET; i flussi live (trace, log) controllano l'origine della console.
- Errori: codici S3 distinti per quorum di lettura e scrittura (`SlowDownRead`, `SlowDownWrite`), lock non
  ottenuto (`XNexoLockTimeout`) e disco o nodo offline (`ServiceUnavailable`); l'Admin API risponde con un
  messaggio curato e registra la catena dell'errore nel log con il request id; la console traduce
  `lock_timeout`, `write_quorum` e `read_quorum`; `nexoctl` formatta gli errori dell'Admin API come quelli S3,
  non ritenta gli errori di quorum, indica `nexoctl admin info` e riporta un server irraggiungibile come
  `cannot reach <url>: <causa>`; `stat`, `cp`, `mv`, `rm` e `share --raw` su un bucket inesistente rispondono
  `bucket not found` (non `object not found`), e il suggerimento sull'alias inesistente compare solo per gli
  argomenti che indicano una posizione, non per i file di input (policy, YAML).
- `nexo server --dry-run` verifica anche `--web-address`, `--web-tls-address` e `--acme-ca-cert`;
  `nexo --version` stampa la versione come `nexo version`.
- `make run-single`/`run-snmd` usano percorsi assoluti; `make run-cluster` mette la console sulla porta S3 + 1;
  `make security` esegue govulncheck e gosec con la toolchain del modulo anche se il `go` installato è più vecchio.
- Docker: `nexo.env.example` senza password predefinita, Prometheus e Grafana dello stack di osservabilità
  legati a `127.0.0.1` e senza credenziali predefinite; in `compose.cluster.yml` nodi e load balancer stanno
  sulla rete predefinita del progetto (con l'indirizzo fisso del load balancer), così l'overlay
  `compose.observability.yml` raggiunge i target di `prometheus.cluster.yml` (verificato in CI).
- CI: workflow puliti per actionlint con shellcheck, lint e vet anche dei test con tag `compose`, `upgrade` e
  `nexotest`.
- Documentazione: rimosse `NEXO_BROWSER` e `NEXO_CONSOLE_SESSION_DURATION`, mai lette (la sessione della console
  dura 12 ore); quickstart allineato all'output reale.

### Aggiornamento
- I formati su disco sono invariati rispetto a `v0.7.0` (`format.json` v1, `xl.meta` 1.x): aggiornamento e
  downgrade verificati in SNMD e con rolling upgrade di un cluster di 4 nodi sotto carico (`make test-upgrade`,
  [08](docs/08-deployment-modes.md)).
- **Prima di riavviare il primo nodo** (cambi di comportamento che rompono un cluster misto o una configurazione
  esistente):
  - con `--tls-internode-verify` i certificati client dei nodi sono verificati solo con le CA di `CAs/` della
    directory dei certificati (e con la foglia di default): una CA presente solo nella catena di `public.crt`,
    accettata da `v0.7.0`, non basta più. Copiare la CA che firma i certificati dei nodi in `CAs/` su **tutti** i
    nodi prima del rolling upgrade, altrimenti ogni nodo aggiornato rifiuta le chiamate internodo dei peer (che
    vedono i suoi dischi offline) fino alla perdita del quorum. `nexo server --dry-run` avvisa quando `CAs/` è
    vuota con `--tls-internode-verify` e una foglia non autofirmata;
  - il listener S3 serve le distribuzioni solo con `--server-url`/`--domain` (`--s3-distributions=auto`, il
    default) o con `--s3-distributions=on`: chi serve distribuzioni sulla porta 9000 senza quelle opzioni deve
    impostare `NEXO_S3_DISTRIBUTIONS=on` (oppure `NEXO_SERVER_URL`/`NEXO_DOMAIN`) prima dell'aggiornamento, o
    spostarle sui listener web (`--web-address`);
  - durante il rolling upgrade le letture coordinate da un nodo aggiornato verso nodi `v0.7.0` (che non hanno
    `storage/open-file`) restano sotto lock fino alla fine, come in `v0.7.0`.

### Limiti noti
- Nessun rate limit per API o per distribuzione (S-OPS-10, S-DST-08 P2): mettere un reverse proxy con limiti
  davanti a Nexo (`deploy/proxy`); `api.requests_max` e `api.requests_deadline` sono salvati ma non applicati.
- Non inclusi (M10): replica di bucket e di sito, decommission e rebalance dei pool, OIDC e LDAP, tiering, S3 Select,
  compressione, ACL diverse da `private`, ACME DNS-01, riavvio coordinato del cluster e aggiornamento orchestrato.
- Pen test della console, O_DIRECT e zero-copy non ancora eseguiti.

### Verifica

Integrazione finale del 2026-10-15 sul working tree (Linux amd64, 4 vCPU, Go 1.26.9, Node.js 22):

| Verifica | Esito | Durata |
|----------|-------|-------:|
| `go mod tidy`, `gofmt -l .` | nessuna modifica, nessun file da formattare | — |
| `go build ./...` (anche `-tags cluster`, `compose`, `proxy`, `upgrade`, `nexotest`) | ok | 32 s |
| `go vet ./...` (anche con i tag) | ok | 54 s |
| `golangci-lint run ./...` v2.14.0 (anche con i tag) | 0 problemi | 35 s |
| `go generate ./...` (mockery v2.53.4) | idempotente | 8 s |
| `govulncheck` v1.1.4 | 0 vulnerabilità raggiunte | 25 s |
| `gosec` v2.29.0, `npm audit --omit=dev` | 0 problemi, 0 vulnerabilità | 75 s |
| `go test -race -count=1 ./...` (59 package, integrazione in SNSD, SNMD e cluster) | ok | 13 min 46 s |
| `make test-cluster` (4 processi `bin/nexo`, TC-01..15) | ok | 4 min 17 s |
| `make test-proxy` (proxy Go, nginx e HAProxy reali) | ok | 28 s |
| `make test-upgrade` (`v0.7.0` → working tree, SNMD e rolling upgrade) | ok | 1 min 4 s |
| `make traceability-check` | 75/75 P0 e 72/72 P1 con test | 2 s |
| `npm run gen:api`, `lint`, `typecheck` | idempotente, ok, ok | 2 s, 52 s, 22 s |
| Vitest | 67 file, 754 test | 1 min 26 s |
| `npm run build`, bundle iniziale | ok, 305,76 kB gzip (limite 307,2) | 47 s |
| `make build-embedded` | ok | 43 s |
| Playwright (`chromium`, `snmd`, `cluster-smoke`, `cluster`) | 118 test | 3 min 8 s |
| `make docs` | 238 pagine del riferimento CLI, deterministiche | 4 s |
