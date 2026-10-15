# Changelog

Tutte le modifiche rilevanti di Nexo. Il formato segue [Keep a Changelog](https://keepachangelog.com/it/1.1.0/) e
le versioni seguono [Semantic Versioning](https://semver.org/lang/it/). I codici tra parentesi (`S-OBJ-06`,
`D-MON-01`, `C-ADM-08`) sono quelli del [catalogo delle funzionalità](docs/02-funzionalita.md); i test che li coprono
sono nella [matrice di tracciabilità](test/TRACEABILITY.md).

## [1.0.0] - da pubblicare

Prima release stabile: milestone M0–M9 della [roadmap](docs/05-roadmap-task.md). Le release precedenti non sono mai
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
  inattesi, in SNSD, SNMD e cluster; differenze volute rispetto ad Amazon S3 in [02 §2.4](docs/02-funzionalita.md).

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
- Il client RPC internodo usa l'HTTP/2 di `net/http` invece di `x/net/http2` (deprecato).
- SIGHUP ricarica solo i certificati TLS; `POST /api/v1/service/restart` riavvia il processo in place.
- Nuove azioni di policy `admin:Profiling` e `admin:Speedtest`, incluse nella policy predefinita `diagnostics`.
- Priorità del catalogo: S-OPS-11, D-MON-03 e C-ADM-12, consegnate in anticipo, passano da P2 a P1.

### Aggiornamento
- I formati su disco sono invariati rispetto a `v0.7.0` (`format.json` v1, `xl.meta` 1.x): aggiornamento e
  downgrade verificati in SNMD e con rolling upgrade di un cluster di 4 nodi sotto carico (`make test-upgrade`,
  [08](docs/08-modalita-deploy.md)).

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
