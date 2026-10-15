# 5. Roadmap e task

Ogni task ha un ID (`T-<milestone>.<n>`), le dipendenze e un **criterio di accettazione (DoD)**.
Regola generale di "Done" per ogni task: codice + test (unit e, dove indicato, integrazione/e2e) verdi in CI,
`golangci-lint` / `eslint` puliti, documentazione aggiornata.

Stime indicative in giorni-persona (gp) per 1 sviluppatore full-stack.

**Vincoli trasversali validi per ogni milestone**
- Ogni funzionalità deve funzionare **sia in singolo nodo sia in cluster** (vedi [08](08-modalita-deploy.md)); la CI esegue le suite in matrice di modalità appena disponibile la modalità corrispondente.
- Ogni milestone produce un **binario** e un'**immagine Docker** funzionanti (artefatti CI) e aggiorna i file `deploy/docker/compose.*.yml`.
- Ogni funzionalità utente è raggiungibile da **API, CLI e dashboard** (salvo funzioni puramente S3).

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
| **M7b** | Regole di accesso regex, domini multipli e distribuzioni (vedi [10](10-domini-accessi-distribuzioni.md)) | 25 gp |
| **M7c** | Cache per dominio (vedi [11](11-cache-domini.md)) | 18 gp |
| **M8** | Osservabilità (metriche, log, audit, trace) + pagine dashboard | 12 gp |
| **M9** | Hardening, prestazioni, compatibilità, packaging → **v1.0** | 15 gp |
| **M10** | P2: replica, site replication, decommission/rebalance, OIDC/LDAP, tiering | 40+ gp |

```
M0 ─▶ M1 ─▶ M2 ─┬─▶ M3 ─┐
                ├─▶ M4 ─┼─▶ M7 ─▶ M8 ─▶ M9 (v1.0) ─▶ M10
                └─▶ M5 ─▶ M6 ─┘
(M7b parte dopo M2 per le access rules e dopo M6 per la propagazione in cluster; precede M8)
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
| T-0.6 | Dockerfile multi-stage (node → go → distroless) per `nexo` e `nexoctl`, sottocomando `nexo healthcheck`, `*_FILE` secrets, `compose.single.yml` | T-0.3 | `docker run` avvia server + console, container `healthy` |
| T-0.8 | Goreleaser: binari multi-OS/arch, immagini multi-arch, build snapshot su ogni PR | T-0.6 | Artefatti scaricabili dalla CI |
| T-0.7 | `internal/testutil`: avvio server in-process su porta casuale con directory temporanee | T-0.5 | Usato dai test di integrazione |

## M1 — S3 core single-drive

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-1.1 | Definizione interfacce `ObjectLayer` e `StorageAPI` + tipi (ObjectInfo, BucketInfo, opzioni) | T-0.1 | Interfacce documentate, mock generato (`mockery`) |
| T-1.0 | Rilevamento modalità da argomenti (SNSD / SNMD / cluster), `mode` in `format.json`, `nexo server --dry-run` | T-0.5 | Test TM-03, TM-12 |
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
| T-2.2 | POST policy (form upload) con tutte le condizioni, redirect, verifica dimensione in streaming (S-OBJ-08b) | T-2.1 | Test TPS-12, TPS-13 |
| T-2.2b | URL presigned completi: GET/PUT/HEAD/DELETE, multipart, `versionId`, override `response-*`, limiti di scadenza, STS token, verifica permessi al momento dell'uso (S-OBJ-08) | T-2.1, T-2.5 | Test TPS-01..11 |
| T-2.3 | Policy engine: parsing JSON AWS, Effect/Action/Resource/Condition (StringEquals, StringLike, IpAddress, Bool, DateGreaterThan…), variabili, deny esplicito prevale | — | Test table-driven > 150 casi + fuzz parser |
| T-2.4 | IAM store su `.nexo.sys/config/iam` cifrato (chiave derivata da root), cache in memoria | T-1.2, T-2.3 | Riavvio preserva utenti/policy |
| T-2.5 | Utenti, policy predefinite e custom, attach (S-IAM-01, 03, 04) | T-2.4 | Utente `readonly` riceve 403 su PUT |
| T-2.6 | Bucket policy + accesso anonimo (S-BKT-03) | T-2.3 | GET anonimo su bucket pubblico 200, privato 403 |
| T-2.7 | Gruppi, service account con policy inline e scadenza (S-IAM-05, 06) | T-2.5 | Policy effettiva = intersezione parent ∩ inline |
| T-2.8 | STS AssumeRole (S-IAM-07) | T-2.7 | Credenziali temporanee scadono correttamente |
| T-2.9 | Share link gestiti e revocabili tramite service account dedicati, elenco/revoca via Admin API (S-OBJ-08c) | T-2.2b, T-2.7 | Test TPS-14 |
| T-2.10 | Accesso anonimo per prefix (private/download/upload/public) generato come bucket policy | T-2.6 | Test TPS-16 |
| T-2.11 | Simulatore di policy + permessi effettivi (S-IAM-04b) | T-2.3 | Test TPS-18 |

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
| T-3.15 | Dialog Condividi, pagina Condivisioni con revoca (D-SHR-01, 03) | T-2.9, T-3.8 | Test TD-P-01, 02 |
| T-3.16 | Tab Accesso del bucket: anonimo per prefix + editor bucket policy (D-POL-01) | T-2.10 | Test TD-P-04 |
| T-3.17 | Generatore upload (PUT/POST policy) e simulatore policy (D-SHR-02, D-POL-03) | T-2.11 | Test TD-P-03, 06 |

## M4 — CLI `nexoctl`

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-4.1 | Struttura cobra, alias config (0600), output tabellare/JSON, codici di uscita, `--debug` | T-0.1 | Golden test help e output |
| T-4.2 | Client S3 (aws-sdk-go-v2) e client admin `pkg/nexoadmin` | T-3.2 | — |
| T-4.3 | `ls`, `mb`, `rb`, `stat`, `cat`, `pipe`, `head` | T-4.2 | testscript e2e |
| T-4.4 | `cp`, `mv`, `rm` con ricorsione, multipart parallelo, progress bar, glob | T-4.3 | Copia 1000 file + 1 file da 2 GiB |
| T-4.5 | `share download/upload/--post/list/revoke`, `presign`, `anonymous`, autocompletamento shell | T-4.3, T-2.9 | Test TCL-P-01..06 |
| T-4.8 | Immagine Docker `nexoctl` e pacchetti binari della CLI | T-0.8 | `docker run nexoctl ls` funziona |
| T-4.6 | `admin info`, `admin user`, `admin policy` | T-4.2 | testscript e2e |
| T-4.7 | `mirror` (+ `--watch` con fsnotify), `find`, `du`, `tree`, `diff` | T-4.4 | Mirror idempotente |

## M5 — Erasure coding single-node

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-5.1 | Parsing endpoint con ellissi, calcolo dimensione set, validazioni | T-0.5 | Test table-driven sulle topologie |
| T-5.2 | `erasure`: encode/decode streaming a blocchi con `reedsolomon`, bitrot HighwayHash per blocco | T-1.1 | Ricostruzione con P shard mancanti |
| T-5.3 | `ErasureObjects`: put/get/delete con quorum, scelta metadati coerenti (`pickValidFileInfo`) | T-5.2 | Test con dischi "naughty" (fault injection) |
| T-5.4 | `ErasureSets` + `ServerPools`, hash di placement | T-5.3 | Distribuzione uniforme (chi-quadro) |
| T-5.9 | CI in matrice SNSD/SNMD per tutte le suite di integrazione, `compose.snmd.yml` | T-5.5 | TM-01 parziale verde |
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
| T-6.10 | `compose.cluster.yml` + nginx, `compose.cluster-tls.yml`, URL pubblico (`NEXO_SERVER_URL`) per presigned dietro proxy | T-6.4 | TM-06, TM-09, TPS-19 |
| T-6.11 | CI in matrice completa SNSD/SNMD/Cluster (integrazione, CLI, Playwright) | T-6.10 | TM-01, TM-02 verdi |
| T-6.12 | `admin cluster export/import` per migrazione tra modalità (S-OPS-15, C-ADM-13): IAM, config e metadati bucket in un solo archivio (oggi `admin config export/import` copre la sola configurazione) | T-6.5 | TM-04 |

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
| T-7.9 | Dashboard: impostazioni bucket, lifecycle, eventi, dettaglio oggetto/versioni, anteprima, gruppi, access key (D-BKT-02..04, D-OBJ-05..06, D-IAM-02, 04). Già in M3/M4: impostazioni bucket (versioning, quota, tag, crittografia predefinita, object lock con retention predefinita), ripristino ed eliminazione versioni, anteprima, gruppi, access key. Restano per M7: lifecycle (D-BKT-03) ed eventi (D-BKT-04), oggi 501 nell'Admin API, retention e legal hold del singolo oggetto (D-OBJ-06) | T-7.1..T-7.7 | E2E per ogni pagina |
| T-7.10 | CLI: `version`, `tag`, `retention`, `legalhold`, `ilm`, `event`, `quota`, `encrypt`, `anonymous`, `admin group`, `admin accesskey`, `admin config`. Già in M3/M4: `version`, `tag`, `quota`, `encrypt`, `anonymous`, `admin group/accesskey/config`. Restano per M7: `retention`, `legalhold`, `ilm`, `event` (con T-7.3, T-7.4, T-7.6) | T-7.1..T-7.7 | testscript per ogni comando |

**Stato della compatibilità S3 (DoD di T-7.1, T-9.1 anticipata per `s3-tests`)** — `make test-s3compat`
(`test/compat`, job notturno `s3tests-nightly` in tutte e tre le modalità, [06 §6.1](06-piano-test.md)) su
`ceph/s3-tests@5522d1c` (`test_s3.py`, 838 test), misurato il 2026-10-09:

- 415 test esclusi, ognuno con il motivo in `test/compat/excluded.txt`: 235 marcati `fails_on_aws` dalla suite
  stessa (estensioni RGW, tra cui il logging), 83 ACL (S-BKT-13, P2), 37 firme V2, 9 PublicAccessBlock (S-ACC-03),
  il resto comportamenti di RGW diversi da S3 o differenze note ([02 §2.4](02-funzionalita.md));
- 422 applicabili: **422/422 in SNMD e cluster, 421/422 in SNSD**; `test_atomic_read_4mb` è fallito in 2 esecuzioni
  SNSD su 4 (mai in SNMD e cluster): il client della suite scrive la chiave mentre non legge ancora la GET della stessa chiave,
  e il lock di lettura tenuto durante il trasferimento (come in MinIO) blocca la PUT finché i buffer dei socket
  non assorbono l'oggetto o scade `NEXO_SEND_TIMEOUT` (20 s). Risolto in T-9.1 (letture su istantanea che
  rilasciano il lock prima del trasferimento, [02 §2.4](02-funzionalita.md)); lo stato attuale, su tutti i file
  della suite e con `mint`, è nello stato di T-9.1 (M9, più sotto);
- gruppi obiettivo di M7 (soglia 90% per gruppo): versioning 28/28, object lock 39/39, multipart 31/31,
  tagging 17/17, SSE-C 18/18, checksum 11/11, CORS 1/1 (13 dei 14 test CORS usano ACL pubbliche o firme V2; il
  CORS è coperto da `TestBucketCORS` nella matrice di integrazione);
- punto di partenza, con le sole esclusioni per marker: 393/592 (66%). Le correzioni (S3 in generale, non
  specifiche della suite) sono coperte da unit test e da `test/integration/compat_test.go` nella matrice TM-01.

## M7b — Accessi, domini e distribuzioni

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-7b.1 | Matcher prefix/glob/regex (RE2) con normalizzazione chiave, limiti, cache compilata | T-2.3 | TAR-01..04, TAR-10 |
| T-7b.2 | Access rules nel policy engine (priorità, condizioni, gruppi nominati), storage in metadati bucket, propagazione peer | T-7b.1, T-6.5 | TAR-05..09 |
| T-7b.3 | Preset pubblici e block public access | T-7b.2 | TAR-08 |
| T-7b.4 | Proxy fidati, `Forwarded`/`X-Forwarded-*`, PROXY protocol, IP reale nelle condizioni | T-0.5 | TDS-08 |
| T-7b.5 | Modello distribuzioni + tabella di routing per Host (esatti, wildcard) aggiornata a caldo | T-6.5 | TDS-01, 02 |
| T-7b.6 | Modalità `website`: index, error page, SPA fallback, redirect, header, CORS, behavior per path | T-7b.5 | TDS-03, 05, 09, 12 |
| T-7b.7 | Modalità `s3` su dominio custom (SigV4/presigned con host pubblico) | T-7b.5 | TDS-04 |
| T-7b.8 | Origin secret per proxy con rotazione, restrizioni IP | T-7b.4 | TDS-07 |
| T-7b.9 | Key group, URL e cookie firmati di distribuzione | T-7b.6 | TDS-06 |
| T-7b.10 | TLS per dominio: SNI, upload, ACME con rinnovo dal leader in cluster; listener `--web-address`/`--web-tls-address` | T-6.6 | TDS-10 |
| T-7b.11 | Admin API + OpenAPI per regole/distribuzioni/certificati/routing resolve | T-7b.2, T-7b.5 | Test di contratto |
| T-7b.12 | Dashboard: regole di accesso con tester, preset, block public (D-ACC-01..04) | T-7b.11 | TD-A-01..03 |
| T-7b.13 | Dashboard: distribuzioni, wizard, behavior con tester, key group, certificati, origin secret (D-DST-01..07) | T-7b.11 | TD-D-01..05 |
| T-7b.14 | CLI `access` e `dist` | T-7b.11 | TCL-A-*, TCL-D-* |
| T-7b.15 | `deploy/proxy/` (nginx, Caddy, Traefik, HAProxy, Cloudflare) + compose con 2 proxy | T-7b.8 | TCD-01..03 (`make test-proxy`: `test/proxy`, proxy Go in-process in ogni modalità e nginx/HAProxy reali con i file di `compose.multi-proxy.yml`) |
| T-7b.16 | Metriche/access log per distribuzione (D-DST-08); la cache è in M7c | T-8.1 | — |

## M7c — Cache per dominio

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-7c.1 | Modello cache policy + ereditarietà globale/distribuzione/dominio/behavior | T-7b.5 | TCC-01, 02 |
| T-7c.2 | Calcolo chiave di cache (query, header, cookie, host, normalizzazione) | T-7c.1 | TCC-03 |
| T-7c.3 | Cache L1 memoria (W-TinyLFU) + L2 disco con indice, quote globali/distribuzione/dominio | T-7c.1 | TCC-13, 14, 16 |
| T-7c.4 | Middleware cache: TTL, stale-while-revalidate/if-error, negative caching, bypass, request collapsing, varianti compresse, header `X-Cache`/`Age` | T-7c.2, T-7c.3 | TCC-04, 07, 08, 10, 11 |
| T-7c.5 | Range caching a blocchi | T-7c.4 | TCC-09 |
| T-7c.6 | Invalidazione automatica dagli eventi di scrittura + manuale (path/glob/regex/tag/dominio/soft), propagazione con numero di sequenza | T-7c.4, T-6.5 | TCC-05, 06, 12, TCC-C-03, 04 |
| T-7c.7 | Modalità cluster `shared` con rendezvous hashing via RPC | T-7c.4, T-6.1 | TCC-C-01, 02 |
| T-7c.8 | Webhook di purge esterni con coda persistente | T-7c.6 | TCC-15 |
| T-7c.9 | Warm-up (prefetch, warmOnWrite) | T-7c.4 | TCL-C-04 |
| T-7c.10 | Ispettore URL, metriche per dominio | T-7c.4, T-8.1 | TCL-C-03 |
| T-7c.11 | Admin API + OpenAPI, CLI `cache` e `dist cache` | T-7c.6 | TCL-C-* |
| T-7c.12 | Dashboard D-CCH-01..08 | T-7c.11 | TD-C-* |
| T-7c.13 | Volume `/cache` nei Compose, flag `--cache-dir/--cache-size`, test compose 2 proxy | T-7c.3 | TCC-C-05 (`make test-proxy`) |

**Stato di M7b e M7c (integrazione del 2026-10-09)**: T-7b.1..T-7b.16 e T-7c.1..T-7c.13 completati in SNSD, SNMD e
cluster. Regole di accesso, block public access, distribuzioni, key group, certificati e configurazione della cache
stanno nel system store erasure-coded e arrivano agli altri nodi con il `cluster.Notifier`, senza restart (TAR-09:
circa 3 ms da nodo1 a nodo4; TCD-03: decine di ms attraverso i proxy).

- **Un solo matcher**: `internal/service/match` (prefix in trie, glob tradotti in RE2, regex RE2, normalizzazione NFC
  di `//`, `.` e `..`) valuta le access rules, i behavior delle distribuzioni (servizio, `dist test` e motore
  website: `distribution.PathPattern` è `match.Pattern`), le impostazioni per percorso, il warm-up e le purge della
  cache; i ban Varnish dei webhook usano l'espressione RE2 del matcher (`Matcher.Expr`) e l'Admin in memoria dei
  test della CLI lo stesso matcher. Un solo `distribution.NormalizeHost` per tabella di routing e cache.
- Misure: TAR-10 p99 per richiesta 5,2 µs con 200 regex (limite 50 µs); risoluzione dell'host 162 ns/op con 11.000
  alias (9.900 esatti, 1.100 wildcard); TCC-16 HIT L1 su 64 KiB p50 4,6 µs, p99 40 µs (limite 1 ms).
- Verifiche dell'integrazione (4 CPU): `go test -race` dei package 3 min 8 s; integrazione (TM-01, con i test
  lenti) SNSD 75 s, SNMD 47 s, cluster 246 s; CLI `test/cli` SNSD 199 s, SNMD 34 s, cluster 230 s; `make
  test-cluster` 249 s; `NEXO_PROXY_REAL=1 make test-proxy` (proxy Go in ogni modalità, nginx e HAProxy reali) 23 s;
  Vitest 62 file / 660 test 72 s; Playwright 112 test (chromium, snmd, cluster) 2,7 min; bundle iniziale 306,25 kB
  gzip (limite 307,2); `golangci-lint` 0 problemi anche con i tag `cluster,compose,proxy`; `go generate` e
  `npm run gen:api` idempotenti.
- Smoke con il binario reale (`bin/nexo` con dashboard, SNMD 4 dischi): distribuzione website su
  `www.example.test` ed `example.test` (header `Host`), MISS poi HIT su entrambi i domini, sei PUT consecutive
  senza mai contenuto vecchio su nessuno dei due, regola regex `deny` su `private/` (403 anche con `%70rivate` e
  `public/../private`, anche per oggetti già in cache), URL firmato valido (200), alterato (403) e scaduto (403).
- Restano P2: rate limit per distribuzione (TDS-13: validato e salvato, non applicato) e challenge ACME DNS-01 per i
  certificati wildcard.

## M8 — Osservabilità

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-8.1 | Metriche Prometheus (API, latenze per istogramma, dischi, rete, heal, scanner, ILM) | M6 | Dashboard Grafana d'esempio in `deploy/` |
| T-8.2 | Ring buffer metriche per i grafici della console + endpoint `/metrics/query` | T-8.1 | — |
| T-8.3 | Trace live e log live aggregati dal cluster via SSE (S-OPS-07) | T-6.5 | `admin trace` mostra richieste di tutti i nodi |
| T-8.4 | Audit log + target webhook/Kafka (S-OPS-06) | T-7.6 | Ogni richiesta autenticata ha un record audit |
| T-8.5 | OpenTelemetry tracing | M1 | Span per richiesta e per RPC |
| T-8.6 | Dashboard Metriche, Log/Trace live, Configurazione (D-MON-01, 02, D-CFG-01); CLI `admin trace/logs` | T-8.2, T-8.3 | E2E |

**Stato di T-8.1–T-8.5 (backend dell'osservabilità), completati.** Dettagli in [07 §7.4](07-sicurezza-operativita.md).
- T-8.1: metriche Prometheus di API (contatori ed istogrammi di latenza per API e bucket), drive, lock, KMS, eventi,
  lifecycle, healing, scanner, RPC internodo, cache e distribuzioni, esposte su `/nexo/metrics/v1/{cluster,node,bucket}`
  (JWT `admin:Prometheus` o pubbliche con `NEXO_PROMETHEUS_AUTH_TYPE=public`), aggregate dal nodo che riceve lo scrape
  per il cluster; dashboard Grafana `nexo-overview`, alert e configurazioni Prometheus in `deploy/docker/observability/`
  (validati da `config_test.go` contro le famiglie esposte).
- T-8.2: ring buffer per nodo (un punto ogni 10 s per 24 h, crescita lazy) e `GET /api/v1/metrics` +
  `/api/v1/metrics/query` con l'aggregato del cluster.
- T-8.3: trace e log live via SSE (`/api/v1/trace`, `/api/v1/logs`) aggregati dai nodi con stream RPC, filtri,
  redazione dei segreti, contatore degli eventi persi senza rallentare le richieste.
- T-8.4: un record di audit per ogni richiesta autenticata (S3 e console) nel log JSON, in memoria (ricerca
  `/api/v1/audit`) e verso i target `audit_webhook`/`audit_kafka` con coda persistente.
- T-8.5: OpenTelemetry OTLP (`http/protobuf` o `grpc`), uno span server per richiesta S3/console/sito/RPC e uno span
  client per chiamata internodo con propagazione `traceparent`; disattivato, costo nullo.
- Integrazione: `TestLiveTrace`, `TestAuditLog`, `TestClusterMetrics` nella matrice SNSD/SNMD/cluster. I ring di
  audit e log crescono su richiesta e il dispatcher dei target di audit parte solo con un target configurato: heap di
  8 cluster di 4 nodi da 328 a 122 MB (la suite di integrazione con `-race` non esaurisce più la memoria).

**Stato di T-8.6.**
- Dashboard: pagina Monitoraggio (`/monitoring/{metrics,trace,logs,audit}`, lazy, sezioni visibili secondo
  `admin:Prometheus`/`admin:ServerInfo`, `admin:ServerTrace`, `admin:ConsoleLog`): grafici di `/metrics/query`
  (richieste, errori, latenze p50/p95/p99, banda, dischi, healing/scanner, internodo, CPU, memoria; cluster o nodo,
  intervallo, auto-refresh, confronto dei nodi), trace e log live via SSE (filtri, pausa/riprendi, buffer limitato,
  riconnessione), ricerca audit; Configurazione (logger, target audit, OTel, TLS) e Info con stato TLS/OTel.
  Vitest (TD-U-11, TD-U-12) e Playwright `e2e/monitoring.spec.ts` (TD-E-16, metriche, audit, permessi, axe).
- CLI (C-ADM-08, C-ADM-12): `nexoctl admin trace|logs` (stream fino a Ctrl-C, `--timeout`, `--count`,
  riconnessione), `admin audit`, `admin metrics list|query`, `admin prometheus generate` (JWT HS512 con
  `iss=prometheus`), `admin speedtest`, `admin profile --seconds`; testscript `test/cli/admin_{trace,observability,
  prometheus,speedtest}.txtar` e `cluster_admin_trace.txtar` (trace di tutti i nodi, DoD di T-8.3).

## M9 — Hardening e rilascio v1.0

| ID | Task | Dipende | DoD |
|----|------|---------|-----|
| T-9.1 | Suite di compatibilità Ceph `s3-tests` e MinIO `mint` in CI notturna. `s3-tests` anticipata in M7 (vedi lo stato di M7), completata con tutti i file della suite, la soglia del 95% su ogni gruppo e `mint` (stato più sotto) | M7 | ≥ 95% dei test applicabili verdi, lista esclusioni motivata |
| T-9.2 | Benchmark con `warp` e profiling (pprof), ottimizzazioni (pool di buffer, O_DIRECT, zero-copy) | M6 | Report prestazioni in `docs/bench/` |
| T-9.3 | Security review: threat model, `gosec`, `govulncheck`, `npm audit`, fuzzing esteso, pen test console | M8 | Nessuna vulnerabilità High aperta |
| T-9.4 | TLS hot reload, SNI multipli (S-OPS-02) | M1 | — |
| T-9.5 | Packaging finale: binari multi-arch, deb/rpm/Homebrew, immagini Docker firmate (cosign), SBOM, Helm chart (`standalone`/`distributed`), unit systemd, stack Swarm | M8 | Release candidata pubblicata, TM-10, TM-11 |
| T-9.6 | Documentazione utente: quickstart, guida cluster, guida operativa, reference CLI generata da cobra | — | Sito docs |
| T-9.7 | Accessibilità e i18n completi dashboard (D-UX-01) | M8 | axe senza violazioni serious/critical |
| T-9.8 | Test di upgrade (v1.0-rc → v1.0) con dati esistenti, rolling restart. Prima della prima release: dalla release di M7 (`224bb31`) al working tree (stato più sotto) | T-9.5 | Nessuna perdita dati |

**Stato di T-9.2, T-9.3, T-9.4 (hardening).**
- T-9.2: benchmark `warp` v1.8.2 in SNSD, SNMD e cluster di 4 processi (`make bench`, `test/bench/warp.sh`),
  profiling via Admin API (`POST /api/v1/profile`, `nexoctl admin profile`) e speed test (`POST /api/v1/speedtest/*`,
  `nexoctl admin speedtest`, S-OPS-11, anticipato da T-10.7); ottimizzazioni misurate (buffer di erasure coding
  riciclati e dimensionati sulla parte, buffer di scrittura dei drive riciclati): report in
  [docs/bench/README.md](bench/README.md). O_DIRECT e zero-copy restano aperti.
- T-9.3: threat model STRIDE per superficie, `make security` (govulncheck, gosec, npm audit) e job CI `security`,
  24 target di fuzzing (`make fuzz`, job notturno `fuzz-nightly`): [docs/security/threat-model.md](security/threat-model.md).
  Con la v1.0 la toolchain è Go 1.26.9 (`go.mod`, Dockerfile, CI con golangci-lint v2.14.0) e i moduli vulnerabili
  sono aggiornati (`x/net` v0.60.0, `x/text` v0.42.0, pgx v5.9.2, OpenTelemetry v1.45.0, grpc v1.83.2,
  `klauspost/compress` v1.18.7, chi v5.3.0): **`govulncheck` senza vulnerabilità raggiunte** (resta GO-2026-5932 di
  `x/crypto`, senza fix e non raggiunta), §10.1 del threat model. Il client RPC internodo usa l'HTTP/2 di `net/http`
  (`http.Transport` con `Protocols`), non più `x/net/http2` deprecato. Manca il pen test della console.
- T-9.4: keystore di `--certs-dir` con SNI, ricarica a caldo (fsnotify e SIGHUP) senza interrompere le connessioni,
  TLS ≥ 1.2 (`--tls-min-version`), catena con i certificati delle distribuzioni ([docs/07 §7.3](07-sicurezza-operativita.md#73-tls)).

**Stato di T-9.5 (packaging).** Configurazione completa e validata in locale, nessuna release pubblicata (la
"release candidata pubblicata" del DoD richiede il primo tag `v1.0.0-rc.1`): [docs/08 §8.7](08-modalita-deploy.md#87-rilascio-t-95).
- GoReleaser v2.18.3 (`goreleaser check` senza deprecazioni; `make release-snapshot` costruisce gli 11 binari
  con la console, 11 archivi + sorgenti, 8 pacchetti deb/rpm `nexo`/`nexoctl`, checksum, 11 SBOM SPDX, cask
  Homebrew); con un demone Docker `make release-snapshot RELEASE_SKIP=sign` costruisce anche le immagini
  linux/amd64 e linux/arm64 (`dockers_v2`), controllate dal job CI `packaging` (architettura, UID 65532,
  `version`, HEALTHCHECK con TLS). Push, attestazioni SBOM/provenance, firme cosign keyless (bundle
  Sigstore) e chart OCI solo nel workflow `release.yml`.
- TM-10: `packaging/smoke.sh` (versione = tag, `--dry-run`, server, console, round trip S3, SIGTERM) verde su
  linux/amd64 e linux/arm64 con QEMU; job `smoke` della release su darwin e windows. TM-11: job `verify`
  (firme di checksum e immagini, attestazioni SBOM, trivy).
- Helm chart `deploy/helm/nexo` (`standalone`/`distributed`, `helm test`), stack Swarm `deploy/swarm`, unit
  systemd con sandbox (esposizione 1,5), job CI `packaging` (snapshot, pacchetti installati, chart, unit,
  Dockerfile, `HEALTHCHECK` dell'immagine con TLS).
- `nexo healthcheck` (HEALTHCHECK delle immagini) segue `NEXO_CERTS_DIR`/`--certs-dir`: HTTPS sulla
  console, fiducia in `public.crt` e `CAs/`; prima un container con TLS restava `unhealthy` (sonda HTTP
  verso un listener TLS) e Swarm ne sostituiva i task. Esiti delle verifiche locali (validato contro
  solo configurato) in [docs/08 §8.7](08-modalita-deploy.md#validato-in-locale-e-solo-configurato).

**Stato di T-9.1 (compatibilità), misurato il 2026-10-15** ([06 §6.1](06-piano-test.md)).
- `ceph/s3-tests@5522d1c`, tutti i file della suite (`test_s3.py`, `test_headers.py`, `test_iam.py`, `test_sts.py`,
  `test_s3select.py`, `test_sns.py`, `test_s3control.py`): 1052 test raccolti, 598 esclusi con motivo
  (`test/compat/excluded.txt`: 265 `rgw`, 146 `p2`, 91 `out-of-catalog`, 47 `deprecated`, 32 `aws`, 11 `design`,
  6 `suite`), 3 saltati dalla suite stessa, **451 applicabili: 451/451 in SNMD, SNSD e cluster**; tutti i 17
  gruppi con test applicabili al 100% (soglia 95%), tra cui object 110, listing 79, object-lock 39, sse-s3-kms 34,
  multipart 31, versioning 28, headers 18, sse-c 18, tagging 17, post-object 12, checksum 10. Revisione delle
  esclusioni: i 168 test esclusi per nome, eseguiti (`NEXO_S3TESTS_CHECK_EXCLUDED=names`, anche nel job
  notturno), falliscono tutti.
- `minio/mint@12559d5` senza Docker (`make test-mint`, job notturno `mint-nightly`), tutte le 14 suite costruite
  ed eseguite, identico in SNMD e cluster: **638 test passati, 0 fallimenti inattesi**, 106 fallimenti attesi
  (`test/compat/mint-excluded.txt`: 67 `minio`, estensioni e Admin API di MinIO; 22 `deprecated`, SigV2 di
  minio-go; 5 `p2`, S3 Select; 4 `out-of-catalog`, checksum MD5; 4 `aws`; 3 `design`; 1 `suite`), 18 NA dichiarati
  dalle suite. Per suite: minio-go 143, minio-js 239, minio-java 109, mc 39, awscli 21, minio-py 18,
  aws-sdk-go-v2 19, versioning 13, aws-sdk-php 11, aws-sdk-ruby 11, s3cmd 8, aws-sdk-java-v2 7; healthcheck
  (endpoint di MinIO) e s3select (P2) senza test applicabili. Le suite che si fermano al primo errore
  (aws-sdk-go-v2, aws-sdk-php, aws-sdk-ruby, mc, minio-py, versioning) non eseguono i test che seguono un
  fallimento atteso.
- Correzioni alla radice emerse dalle due suite (unit test e `test/integration/compat_test.go` nella matrice):
  letture su istantanea (`test_atomic_read_4mb`), `DeleteObject` condizionale con `If-Match` (412; 501 per le
  condizioni dei soli directory bucket), `NoSuchVersion` per un `versionId` di una chiave inesistente,
  `PutBucketTagging` 200, `NextPartNumberMarker` sull'ultima pagina di `ListParts`/`GetObjectAttributes`,
  elemento `Type` del grantee nelle ACL, `x-amz-content-sha256` malformato nelle presigned, campo file senza nome
  nei POST (`MalformedPOSTRequest`) e checksum nella risposta, CORS (header in minuscolo, richieste non preflight
  con `Access-Control-Request-Method`), `POST` su un oggetto senza sottorisorsa 405, algoritmi di checksum non
  supportati rifiutati, apici non codificati nei messaggi di errore XML ([02 §2.4](02-funzionalita.md)).

**Stato di T-9.8 (upgrade), misurato il 2026-10-15.** Nessuna release è ancora stata pubblicata: la release
precedente è il commit di M7 `224bb31` (costruito come `v0.7.0`), la release in prova il working tree (`v0.9.0`);
`make test-upgrade` (job notturno `upgrade-nightly`) verde in 46 s.
- `TestUpgradeSNMD`: 14 versioni di 10 chiavi scritte da `v0.7.0` (versioni e delete marker, multipart CRC32C e
  un upload aperto, SSE-S3/KMS/C, cifratura predefinita, Object Lock, tag, IAM, bucket policy, lifecycle, CORS,
  notifiche con un evento accodato) rilette da `v0.9.0` (sha256 di ogni versione), evento accodato recapitato,
  dati modificati (18 versioni); formati invariati (`format.json` v1, `xl.meta` 1.x), quindi downgrade verificato:
  `v0.7.0` serve i dischi modificati dalla nuova e vi scrive, `v0.9.0` rilegge tutto (21 versioni di 15 chiavi).
- `TestRollingUpgradeCluster`: 4 nodi × 2 dischi aggiornati uno alla volta (1,6–2,7 s per nodo) sotto carico
  (1326 operazioni di 8 worker): 0 errori client oltre ai retry dell'SDK, 0 letture incoerenti; rollback di un
  nodo a `v0.7.0` nel cluster aggiornato e ritorno a `v0.9.0` senza perdita dati.
- Politica di downgrade: [08, aggiornamenti e downgrade](08-modalita-deploy.md).

**Stato di T-9.7 (accessibilità e i18n).** `@axe-core/playwright` su ogni pagina e sezione della console
(TD-E-19: `a11y.spec.ts`, `access-distributions-a11y.spec.ts`, sezioni di cache e monitoraggio) senza violazioni
serious/critical, anche in tema scuro; it/en senza chiavi mancanti (`i18n/parity.test.ts`, TD-U-13); tema scuro,
inglese e viewport 375 px (TD-E-18). I contrasti dei badge di stato sono stati scuriti in `web/src/theme.ts`.

**Tracciabilità (docs/06 §6.8).** `test/TRACEABILITY.md`, generata da `make traceability` (`cmd/tools/traceability`,
logica e test in `internal/tools/traceability`, fuori dai binari di rilascio) dai tag `covers: <codice>` dei test Go,
degli script `testscript`, dei test Vitest/Playwright e degli script di rilascio: **75/75 funzionalità P0 e 72/72 P1
con almeno un test**; le 18 P2 (M10) sono elencate come "non in v1.0". `make traceability-check` (job `lint-go` e
workflow di release) e il test `TestRepositoryTraceability` falliscono per una P0/P1 senza test, per un codice fuori
catalogo o per la matrice non aggiornata. Tre P2 consegnate in anticipo sono diventate P1 nel catalogo (S-OPS-11,
D-MON-03, C-ADM-12, [02](02-funzionalita.md)).

**Verifica finale della v1.0 (integrazione, 2026-10-15).** Esiti e durate nel [CHANGELOG](../CHANGELOG.md#verifica);
tutto verde in SNSD, SNMD e cluster. Restano aperti, fuori dal perimetro P0/P1: pen test della console (T-9.3),
O_DIRECT e zero-copy (T-9.2), pubblicazione della prima release candidata `v1.0.0-rc.1` con firme e immagini (T-9.5,
richiede il tag), rate limit per API e per distribuzione e applicazione di `api.requests_max`/`api.requests_deadline`
(S-OPS-10, S-DST-08, M10), ACME DNS-01, riavvio coordinato del cluster e aggiornamento rolling orchestrato (S-OPS-09).

## M10 — Post v1.0 (P2)

| ID | Task |
|----|------|
| T-10.1 | Bucket replication attivo-attivo + resync (S-BKT-12) |
| T-10.2 | Site replication (S-STO-11), CLI `admin replicate`, pagina dashboard |
| T-10.3 | Decommission pool (S-STO-09) e rebalance (S-STO-10) |
| T-10.4 | OIDC (login console SSO + AssumeRoleWithWebIdentity) e LDAP (S-IAM-08, 09) |
| T-10.5 | Tiering ILM verso S3 remoto (S-STO-12) |
| T-10.6 | SelectObjectContent, compressione trasparente (GetObjectAttributes anticipata in M7) |
| T-10.7 | Rate limiting, speedtest, rolling upgrade orchestrato (`admin update`) |
| T-10.8 | Operatore Kubernetes |
