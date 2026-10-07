# 5. Roadmap e task

Ogni task ha un ID (`T-<milestone>.<n>`), le dipendenze e un **criterio di accettazione (DoD)**.
Regola generale di "Done" per ogni task: codice + test (unit e, dove indicato, integrazione/e2e) verdi in CI,
`golangci-lint` / `eslint` puliti, documentazione aggiornata.

Stime indicative in giorni-persona (gp) per 1 sviluppatore full-stack.

## Panoramica milestone

| Milestone | Obiettivo | Stima |
|-----------|-----------|-------|
| **M0** | Fondamenta: repo, build, CI, scheletro server/CLI/web | 5 gp |
| **M1** | Server S3 single-node single-drive (P0 core) | 20 gp |
| **M2** | Autenticazione SigV4, IAM base, policy | 15 gp |
| **M3** | Admin API + Dashboard MVP embedded | 20 gp |
| **M4** | CLI `nexoctl` MVP | 12 gp |
| **M5** | Erasure coding multi-drive su singolo nodo, bitrot, healing locale | 20 gp |
| **M6** | Cluster distribuito (RPC, dsync, bootstrap, pool) | 30 gp |
| **M7** | Funzionalità avanzate (versioning, lifecycle, object lock, SSE, eventi, quota) | 30 gp |
| **M8** | Osservabilità (metriche, log, audit, trace) + pagine dashboard | 12 gp |
| **M9** | Hardening, prestazioni, compatibilità, packaging → **v1.0** | 15 gp |
| **M10** | P2: replica, site replication, decommission/rebalance, OIDC/LDAP, tiering | 40+ gp |

```
M0 ─▶ M1 ─▶ M2 ─┬─▶ M3 ─┐
                ├─▶ M4 ─┼─▶ M7 ─▶ M8 ─▶ M9 (v1.0) ─▶ M10
                └─▶ M5 ─▶ M6 ─┘
```
M3/M4 (UI e CLI) e M5 (storage) possono procedere in parallelo grazie all'interfaccia `ObjectLayer`.

---

## M0 — Fondamenta

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-0.1 | Init `go.mod` (`github.com/r3n3r0/nexo`), layout cartelle, `Makefile` (`build`, `test`, `lint`, `web`, `e2e`) | — | `make build` produce `bin/nexo` e `bin/nexoctl` |
| T-0.2 | Scaffold `web/` con Vite + React + TS + Mantine + ESLint + Prettier + Vitest | — | `npm run build` e `npm test` verdi |
| T-0.3 | `go:embed` della dashboard con build tag `embedui`, fallback SPA, header di cache, CSP | T-0.1, T-0.2 | `GET /` serve index, `GET /buckets/x` → index, asset con cache immutable |
| T-0.4 | CI GitHub Actions: lint, unit Go (con `-race`), unit web, build, upload artefatti | T-0.1, T-0.2 | Pipeline verde su PR |
| T-0.5 | Config loader (flag/env/file) con `cobra`, logging `slog`, graceful shutdown | T-0.1 | `nexo server --help`, SIGTERM chiude in < 10 s |
| T-0.6 | Dockerfile multi-stage (node → go → distroless), `docker-compose.yml` dev | T-0.3 | `docker run` avvia server + console |
| T-0.7 | `internal/testutil`: avvio server in-process su porta casuale con directory temporanee | T-0.5 | Usato dai test di integrazione |

## M1 — S3 core single-drive

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-1.1 | Definizione interfacce `ObjectLayer` e `StorageAPI` + tipi (ObjectInfo, BucketInfo, opzioni) | T-0.1 | Interfacce documentate, mock generato (`mockery`) |
| T-1.2 | `xlStorage`: layout su disco, `format.json`, scrittura atomica tmp+rename+fsync, pulizia tmp all'avvio | T-1.1 | Test crash-consistency (kill durante scrittura) |
| T-1.3 | Formato `xl.meta` con msgpack (versioni, inline data), codegen `msgp`, fuzz test di decodifica | T-1.2 | Round-trip + fuzz 10 min senza panic |
| T-1.4 | Router S3: path-style e virtual-host, parsing query/header, mapping errori XML | T-0.5 | Tabella errori completa testata |
| T-1.5 | Bucket API (S-BKT-01, 02) | T-1.2, T-1.4 | aws-sdk-go-v2 integration test verde |
| T-1.6 | Put/Get/Head/Delete object, metadati, range, conditional (S-OBJ-01, 02, 07) | T-1.5 | Test integrazione + ETag MD5 corretto |
| T-1.7 | ListObjects V1/V2 con walk ordinato, delimiter, paginazione (S-OBJ-03) | T-1.6 | 100k oggetti listati correttamente con continuation token |
| T-1.8 | CopyObject, DeleteObjects (S-OBJ-04, 05) | T-1.6 | Test integrazione |
| T-1.9 | Multipart completo (S-OBJ-06), ETag multipart `md5-of-md5s-N`, pulizia upload stale | T-1.6 | Upload 5 GiB con 100 parti, parti fuori ordine, abort |
| T-1.10 | Health endpoint e server info base (S-OPS-03, 08) | T-0.5 | `/nexo/health/ready` 200 |

## M2 — Autenticazione e IAM

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-2.1 | SigV4: header, presigned query, `UNSIGNED-PAYLOAD`, streaming `aws-chunked` con firma per chunk e trailer (S-IAM-02) | T-1.4 | Vettori di test ufficiali AWS SigV4 test suite verdi |
| T-2.2 | POST policy (form upload) (S-OBJ-08) | T-2.1 | Test con form generato da SDK |
| T-2.3 | Policy engine: parsing JSON AWS, Effect/Action/Resource/Condition (StringEquals, StringLike, IpAddress, Bool, DateGreaterThan…), variabili, deny esplicito prevale | — | Test table-driven > 150 casi + fuzz parser |
| T-2.4 | IAM store su `.nexo.sys/config/iam` cifrato (chiave derivata da root), cache in memoria | T-1.2, T-2.3 | Riavvio preserva utenti/policy |
| T-2.5 | Utenti, policy predefinite e custom, attach (S-IAM-01, 03, 04) | T-2.4 | Utente `readonly` riceve 403 su PUT |
| T-2.6 | Bucket policy + accesso anonimo (S-BKT-03) | T-2.3 | GET anonimo su bucket pubblico 200, privato 403 |
| T-2.7 | Gruppi, service account con policy inline e scadenza (S-IAM-05, 06) | T-2.5 | Policy effettiva = intersezione parent ∩ inline |
| T-2.8 | STS AssumeRole (S-IAM-07) | T-2.7 | Credenziali temporanee scadono correttamente |

## M3 — Admin API + Dashboard MVP

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-3.1 | Spec OpenAPI `api/openapi.yaml`, generazione client TS, lint spec (spectral) | T-0.2 | Client generato in CI, diff = errore |
| T-3.2 | Admin API server: routing, auth SigV4 + sessione, `problem+json`, autorizzazione `admin:*` | T-2.5 | Test contratto vs OpenAPI |
| T-3.3 | Sessione console: login → JWT in cookie `HttpOnly; Secure; SameSite=Strict`, CSRF double-submit, scadenza, refresh | T-3.2 | Test sicurezza sessione (vedi piano test) |
| T-3.4 | Shell UI: layout, sidebar, routing, tema, i18n, gestione errori globale, guard di autenticazione | T-0.2 | Navigazione e redirect a login testati |
| T-3.5 | Pagina Login (D-AUTH-01) | T-3.3, T-3.4 | E2E login OK/KO/logout |
| T-3.6 | Overview (D-DASH-01) | T-3.2 | Mostra dati reali da `/info` e `/usage` |
| T-3.7 | Lista bucket + crea/elimina (D-BKT-01) | T-3.2 | E2E crea/elimina bucket |
| T-3.8 | Object browser con listing virtualizzato (D-OBJ-01) | T-3.7 | 10k oggetti scorrono a 60 fps |
| T-3.9 | Upload multipart dal browser con coda, progress, annulla, cartelle (D-OBJ-02) | T-3.8 | E2E upload 200 MiB + annullamento |
| T-3.10 | Download singolo e zip streaming lato server (D-OBJ-03) | T-3.8 | Zip valido con 1000 file |
| T-3.11 | Azioni oggetto: elimina, elimina prefix, crea cartella, share presigned (D-OBJ-04) | T-3.8 | E2E |
| T-3.12 | Utenti e Policy con editor Monaco + validazione server (D-IAM-01, 03) | T-3.2 | E2E crea utente, assegna policy, login con nuovo utente |
| T-3.13 | UI permission-aware tramite `/me/permissions` (D-UX-02) | T-3.12 | Utente readonly non vede pulsanti di scrittura |
| T-3.14 | Pagina Info (D-CFG-02) | T-3.2 | — |

## M4 — CLI `nexoctl`

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-4.1 | Struttura cobra, alias config (0600), output tabellare/JSON, codici di uscita, `--debug` | T-0.1 | Golden test help e output |
| T-4.2 | Client S3 (aws-sdk-go-v2) e client admin `pkg/nexoadmin` | T-3.2 | — |
| T-4.3 | `ls`, `mb`, `rb`, `stat`, `cat`, `pipe`, `head` | T-4.2 | testscript e2e |
| T-4.4 | `cp`, `mv`, `rm` con ricorsione, multipart parallelo, progress bar, glob | T-4.3 | Copia 1000 file + 1 file da 2 GiB |
| T-4.5 | `share`, autocompletamento shell | T-4.3 | — |
| T-4.6 | `admin info`, `admin user`, `admin policy` | T-4.2 | testscript e2e |
| T-4.7 | `mirror` (+ `--watch` con fsnotify), `find`, `du`, `tree`, `diff` | T-4.4 | Mirror idempotente |

## M5 — Erasure coding single-node

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-5.1 | Parsing endpoint con ellissi, calcolo dimensione set, validazioni | T-0.5 | Test table-driven sulle topologie |
| T-5.2 | `erasure`: encode/decode streaming a blocchi con `reedsolomon`, bitrot HighwayHash per blocco | T-1.1 | Ricostruzione con P shard mancanti |
| T-5.3 | `ErasureObjects`: put/get/delete con quorum, scelta metadati coerenti (`pickValidFileInfo`) | T-5.2 | Test con dischi "naughty" (fault injection) |
| T-5.4 | `ErasureSets` + `ServerPools`, hash di placement | T-5.3 | Distribuzione uniforme (chi-quadro) |
| T-5.5 | Listing merge da più dischi con quorum | T-5.4 | Listing coerente con 1 disco offline |
| T-5.6 | Heal-on-read + MRF + heal oggetto/bucket | T-5.3 | Disco svuotato → ripopolato |
| T-5.7 | Disk monitor, sostituzione disco, `healing.bin` con ripresa | T-5.6 | Healing riprende dopo restart |
| T-5.8 | Scanner in background (usage + heal a campione) con throttling | T-5.6 | Usage corretto in `/usage` |

## M6 — Cluster distribuito

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-6.1 | Framework RPC internode (HTTP/2, msgpack, streaming, JWT internode, timeout, reconnect) | T-0.5 | Benchmark ≥ 1 GB/s su loopback |
| T-6.2 | `storageRESTServer/Client` che implementa `StorageAPI` in remoto | T-6.1, T-5.3 | Stessa suite di test di `xlStorage` passa su remoto |
| T-6.3 | Lock server + client `dsync` con quorum, refresh, scadenza | T-6.1 | Test con nodi che crashano tenendo lock |
| T-6.4 | Bootstrap: verifica topologia, attesa quorum, formattazione coordinata | T-6.2 | Avvio in ordine casuale dei nodi |
| T-6.5 | Peer RPC: invalidazione cache IAM/bucket/config, server info aggregata | T-6.1 | Utente creato su nodo1 utilizzabile subito su nodo3 |
| T-6.6 | Leader election via lock per job singleton (scanner, ILM) | T-6.3 | Un solo scanner attivo nel cluster |
| T-6.7 | Espansione con nuovi pool + selezione pool pesata | T-5.4 | Nuovi oggetti vanno nel pool vuoto |
| T-6.8 | Ambiente `test/cluster` docker-compose 4×4 + toxiproxy, suite chaos | T-6.4 | Suite chaos verde in CI notturna |
| T-6.9 | Dashboard pagina Cluster e Healing (D-CLU-01, 02), CLI `admin heal` (C-ADM-07) | T-6.5 | E2E su cluster docker |

## M7 — Funzionalità avanzate

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-7.1 | Versioning, delete marker, ListObjectVersions (S-BKT-04, S-OBJ-10) | M5 | s3-tests versioning verdi |
| T-7.2 | Tagging bucket/oggetti (S-BKT-05, S-OBJ-11), CORS (S-BKT-06) | M1 | — |
| T-7.3 | Lifecycle: parsing, valutazione nello scanner, expiration/noncurrent/abort MPU (S-BKT-07) | T-5.8, T-7.1 | Oggetti scadono con clock simulato |
| T-7.4 | Object lock: retention, legal hold, governance bypass (S-BKT-08, S-OBJ-12) | T-7.1 | Delete bloccata in compliance anche per root |
| T-7.5 | KMS interno (master key da env) + interfaccia KMS esterna; SSE-S3/SSE-C/SSE-KMS formato DARE (S-OBJ-13, S-BKT-11) | M1 | Dati su disco non leggibili in chiaro; range GET su oggetti cifrati |
| T-7.6 | Notifiche eventi: target Webhook, Kafka, NATS, AMQP, Redis, PostgreSQL; coda persistente per target offline (S-BKT-10) | M1 | Evento recapitato dopo ripristino target |
| T-7.7 | Quota bucket (S-BKT-09) | T-5.8 | PUT oltre quota → 507/`XNexoQuotaExceeded` |
| T-7.8 | Checksum S3 aggiuntivi (S-OBJ-09) | T-1.6 | — |
| T-7.9 | Dashboard: impostazioni bucket, lifecycle, eventi, dettaglio oggetto/versioni, anteprima, gruppi, access key (D-BKT-02..04, D-OBJ-05..06, D-IAM-02, 04) | T-7.1..T-7.7 | E2E per ogni pagina |
| T-7.10 | CLI: `version`, `tag`, `retention`, `legalhold`, `ilm`, `event`, `quota`, `encrypt`, `anonymous`, `admin group`, `admin accesskey`, `admin config` | T-7.1..T-7.7 | testscript per ogni comando |

## M8 — Osservabilità

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-8.1 | Metriche Prometheus (API, latenze per istogramma, dischi, rete, heal, scanner, ILM) | M6 | Dashboard Grafana d'esempio in `deploy/` |
| T-8.2 | Ring buffer metriche per i grafici della console + endpoint `/metrics/query` | T-8.1 | — |
| T-8.3 | Trace live e log live aggregati dal cluster via SSE (S-OPS-07) | T-6.5 | `admin trace` mostra richieste di tutti i nodi |
| T-8.4 | Audit log + target webhook/Kafka (S-OPS-06) | T-7.6 | Ogni richiesta autenticata ha un record audit |
| T-8.5 | OpenTelemetry tracing | M1 | Span per richiesta e per RPC |
| T-8.6 | Dashboard Metriche, Log/Trace live, Configurazione (D-MON-01, 02, D-CFG-01); CLI `admin trace/logs` | T-8.2, T-8.3 | E2E |

## M9 — Hardening e rilascio v1.0

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-9.1 | Suite di compatibilità Ceph `s3-tests` e MinIO `mint` in CI notturna | M7 | ≥ 95% dei test applicabili verdi, lista esclusioni motivata |
| T-9.2 | Benchmark con `warp` e profiling (pprof), ottimizzazioni (pool di buffer, O_DIRECT, zero-copy) | M6 | Report prestazioni in `docs/bench/` |
| T-9.3 | Security review: threat model, `gosec`, `govulncheck`, `npm audit`, fuzzing esteso, pen test console | M8 | Nessuna vulnerabilità High aperta |
| T-9.4 | TLS hot reload, SNI multipli (S-OPS-02) | M1 | — |
| T-9.5 | Packaging: binari multi-arch (goreleaser), immagini Docker firmate (cosign), SBOM, Helm chart, unit systemd | M8 | Release candidata pubblicata |
| T-9.6 | Documentazione utente: quickstart, guida cluster, guida operativa, reference CLI generata da cobra | — | Sito docs |
| T-9.7 | Accessibilità e i18n completi dashboard (D-UX-01) | M8 | axe senza violazioni serious/critical |
| T-9.8 | Test di upgrade (v1.0-rc → v1.0) con dati esistenti, rolling restart | T-9.5 | Nessuna perdita dati |

## M10 — Post v1.0 (P2)

| ID | Task |
|----|------|
| T-10.1 | Bucket replication attivo-attivo + resync (S-BKT-12) |
| T-10.2 | Site replication (S-STO-11), CLI `admin replicate`, pagina dashboard |
| T-10.3 | Decommission pool (S-STO-09) e rebalance (S-STO-10) |
| T-10.4 | OIDC (login console SSO + AssumeRoleWithWebIdentity) e LDAP (S-IAM-08, 09) |
| T-10.5 | Tiering ILM verso S3 remoto (S-STO-12) |
| T-10.6 | SelectObjectContent, compressione trasparente, GetObjectAttributes |
| T-10.7 | Rate limiting, speedtest, rolling upgrade orchestrato (`admin update`) |
| T-10.8 | Operatore Kubernetes |
