# 5. Roadmap and tasks

Every task has an ID (`T-<milestone>.<n>`), its dependencies and an **acceptance criterion (DoD)**.
General "Done" rule for every task: code + tests (unit and, where stated, integration/e2e) green in CI,
`golangci-lint` / `eslint` clean, documentation updated.

Indicative estimates in person-days (pd) for 1 full-stack developer.

**Cross-cutting constraints valid for every milestone**
- Every feature must work **both on a single node and in a cluster** (see [08](08-deployment-modes.md)); CI runs the suites in a mode matrix as soon as the corresponding mode is available.
- Every milestone produces a working **binary** and **Docker image** (CI artifacts) and updates the `deploy/docker/compose.*.yml` files.
- Every user feature is reachable from the **API, the CLI and the dashboard** (except purely S3 functions).

## Milestone overview

| Milestone | Goal | Estimate |
|-----------|------|----------|
| **M0** | Foundations: repo, build, CI, server/CLI/web skeleton | 5 pd |
| **M1** | Single-node single-drive S3 server (P0 core) | 20 pd |
| **M2** | SigV4 authentication, basic IAM, policies | 15 pd |
| **M3** | Admin API + embedded MVP dashboard | 20 pd |
| **M4** | `nexoctl` MVP CLI | 12 pd |
| **M5** | Multi-drive erasure coding on a single node, bitrot, local healing | 20 pd |
| **M6** | Distributed cluster (RPC, dsync, bootstrap, pools) | 30 pd |
| **M7** | Advanced features (versioning, lifecycle, object lock, SSE, events, quota) | 30 pd |
| **M7b** | Regex access rules, multiple domains and distributions (see [10](10-domains-access-distributions.md)) | 25 pd |
| **M7c** | Per-domain cache (see [11](11-domain-cache.md)) | 18 pd |
| **M8** | Observability (metrics, logs, audit, trace) + dashboard pages | 12 pd |
| **M9** | Hardening, performance, compatibility, packaging → **v1.0** | 15 pd |
| **M10** | P2: replication, site replication, decommission/rebalance, OIDC/LDAP, tiering | 40+ pd |

```
M0 ─▶ M1 ─▶ M2 ─┬─▶ M3 ─┐
                ├─▶ M4 ─┼─▶ M7 ─▶ M8 ─▶ M9 (v1.0) ─▶ M10
                └─▶ M5 ─▶ M6 ─┘
(M7b starts after M2 for the access rules and after M6 for cluster propagation; it precedes M8)
```
M3/M4 (UI and CLI) and M5 (storage) can proceed in parallel thanks to the `ObjectLayer` interface.

---

## M0 — Foundations

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-0.1 | Init `go.mod` (`github.com/r3n3r0/nexo`), folder layout, `Makefile` (`build`, `test`, `lint`, `web`, `e2e`) | — | `make build` produces `bin/nexo` and `bin/nexoctl` |
| T-0.2 | Scaffold `web/` with Vite + React + TS + Mantine + ESLint + Prettier + Vitest | — | `npm run build` and `npm test` green |
| T-0.3 | `go:embed` of the dashboard with the `embedui` build tag, SPA fallback, cache headers, CSP | T-0.1, T-0.2 | `GET /` serves index, `GET /buckets/x` → index, assets with immutable cache |
| T-0.4 | GitHub Actions CI: lint, Go unit tests (with `-race`), web unit tests, build, artifact upload | T-0.1, T-0.2 | Pipeline green on PRs |
| T-0.5 | Config loader (flags/env/file) with `cobra`, `slog` logging, graceful shutdown | T-0.1 | `nexo server --help`, SIGTERM shuts down in < 10 s |
| T-0.6 | Multi-stage Dockerfile (node → go → distroless) for `nexo` and `nexoctl`, `nexo healthcheck` subcommand, `*_FILE` secrets, `compose.single.yml` | T-0.3 | `docker run` starts server + console, container `healthy` |
| T-0.8 | Goreleaser: multi-OS/arch binaries, multi-arch images, snapshot build on every PR | T-0.6 | Artifacts downloadable from CI |
| T-0.7 | `internal/testutil`: in-process server start on a random port with temporary directories | T-0.5 | Used by the integration tests |

## M1 — Single-drive S3 core

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-1.1 | `ObjectLayer` and `StorageAPI` interface definitions + types (ObjectInfo, BucketInfo, options) | T-0.1 | Documented interfaces, generated mock (`mockery`) |
| T-1.0 | Mode detection from arguments (SNSD / SNMD / cluster), `mode` in `format.json`, `nexo server --dry-run` | T-0.5 | Tests TM-03, TM-12 |
| T-1.2 | `xlStorage`: on-disk layout, `format.json`, atomic tmp+rename+fsync writes, tmp cleanup at startup | T-1.1 | Crash-consistency test (kill during a write) |
| T-1.3 | `xl.meta` format with msgpack (versions, inline data), `msgp` codegen, decoding fuzz test | T-1.2 | Round trip + 10 min fuzz without panics |
| T-1.4 | S3 router: path-style and virtual-host, query/header parsing, XML error mapping | T-0.5 | Complete error table tested |
| T-1.5 | Bucket API (S-BKT-01, 02) | T-1.2, T-1.4 | aws-sdk-go-v2 integration test green |
| T-1.6 | Put/Get/Head/Delete object, metadata, range, conditional (S-OBJ-01, 02, 07) | T-1.5 | Integration tests + correct MD5 ETag |
| T-1.7 | ListObjects V1/V2 with ordered walk, delimiter, pagination (S-OBJ-03) | T-1.6 | 100k objects listed correctly with continuation token |
| T-1.8 | CopyObject, DeleteObjects (S-OBJ-04, 05) | T-1.6 | Integration tests |
| T-1.9 | Complete multipart (S-OBJ-06), multipart ETag `md5-of-md5s-N`, cleanup of stale uploads | T-1.6 | 5 GiB upload with 100 parts, out-of-order parts, abort |
| T-1.10 | Health endpoint and basic server info (S-OPS-03, 08) | T-0.5 | `/nexo/health/ready` 200 |

## M2 — Authentication and IAM

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-2.1 | SigV4: header, presigned query, `UNSIGNED-PAYLOAD`, `aws-chunked` streaming with per-chunk signature and trailer (S-IAM-02) | T-1.4 | Official AWS SigV4 test suite vectors green |
| T-2.2 | POST policy (form upload) with all the conditions, redirect, streaming size check (S-OBJ-08b) | T-2.1 | Tests TPS-12, TPS-13 |
| T-2.2b | Complete presigned URLs: GET/PUT/HEAD/DELETE, multipart, `versionId`, `response-*` overrides, expiry limits, STS token, permission check at time of use (S-OBJ-08) | T-2.1, T-2.5 | Tests TPS-01..11 |
| T-2.3 | Policy engine: AWS JSON parsing, Effect/Action/Resource/Condition (StringEquals, StringLike, IpAddress, Bool, DateGreaterThan…), variables, explicit deny wins | — | Table-driven tests > 150 cases + parser fuzzing |
| T-2.4 | IAM store in encrypted `.nexo.sys/config/iam` (key derived from root), in-memory cache | T-1.2, T-2.3 | Restart preserves users/policies |
| T-2.5 | Users, built-in and custom policies, attach (S-IAM-01, 03, 04) | T-2.4 | A `readonly` user gets 403 on PUT |
| T-2.6 | Bucket policy + anonymous access (S-BKT-03) | T-2.3 | Anonymous GET on a public bucket 200, private 403 |
| T-2.7 | Groups, service accounts with inline policy and expiry (S-IAM-05, 06) | T-2.5 | Effective policy = parent ∩ inline intersection |
| T-2.8 | STS AssumeRole (S-IAM-07) | T-2.7 | Temporary credentials expire correctly |
| T-2.9 | Managed, revocable share links through dedicated service accounts, listing/revocation via the Admin API (S-OBJ-08c) | T-2.2b, T-2.7 | Test TPS-14 |
| T-2.10 | Per-prefix anonymous access (private/download/upload/public) generated as a bucket policy | T-2.6 | Test TPS-16 |
| T-2.11 | Policy simulator + effective permissions (S-IAM-04b) | T-2.3 | Test TPS-18 |

## M3 — Admin API + MVP dashboard

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-3.1 | OpenAPI spec `api/openapi.yaml`, TS client generation, spec lint (spectral) | T-0.2 | Client generated in CI, diff = error |
| T-3.2 | Admin API server: routing, SigV4 + session auth, `problem+json`, `admin:*` authorization | T-2.5 | Contract tests vs OpenAPI |
| T-3.3 | Console session: login → JWT in an `HttpOnly; Secure; SameSite=Strict` cookie, double-submit CSRF, expiry, refresh | T-3.2 | Session security tests (see the test plan) |
| T-3.4 | UI shell: layout, sidebar, routing, theme, i18n, global error handling, authentication guard | T-0.2 | Navigation and redirect to login tested |
| T-3.5 | Login page (D-AUTH-01) | T-3.3, T-3.4 | E2E login OK/KO/logout |
| T-3.6 | Overview (D-DASH-01) | T-3.2 | Shows real data from `/info` and `/usage` |
| T-3.7 | Bucket list + create/delete (D-BKT-01) | T-3.2 | E2E create/delete bucket |
| T-3.8 | Object browser with virtualized listing (D-OBJ-01) | T-3.7 | 10k objects scroll at 60 fps |
| T-3.9 | Multipart upload from the browser with queue, progress, cancel, folders (D-OBJ-02) | T-3.8 | E2E 200 MiB upload + cancellation |
| T-3.10 | Single download and server-side streaming zip (D-OBJ-03) | T-3.8 | Valid zip with 1000 files |
| T-3.11 | Object actions: delete, delete prefix, create folder, presigned share (D-OBJ-04) | T-3.8 | E2E |
| T-3.12 | Users and Policies with Monaco editor + server-side validation (D-IAM-01, 03) | T-3.2 | E2E create user, assign policy, login with the new user |
| T-3.13 | Permission-aware UI through `/me/permissions` (D-UX-02) | T-3.12 | A readonly user does not see write buttons |
| T-3.14 | Info page (D-CFG-02) | T-3.2 | — |
| T-3.15 | Share dialog, Shares page with revocation (D-SHR-01, 03) | T-2.9, T-3.8 | Tests TD-P-01, 02 |
| T-3.16 | Bucket Access tab: per-prefix anonymous access + bucket policy editor (D-POL-01) | T-2.10 | Test TD-P-04 |
| T-3.17 | Upload generator (PUT/POST policy) and policy simulator (D-SHR-02, D-POL-03) | T-2.11 | Tests TD-P-03, 06 |

## M4 — `nexoctl` CLI

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-4.1 | cobra structure, alias config (0600), table/JSON output, exit codes, `--debug` | T-0.1 | Golden tests of help and output |
| T-4.2 | S3 client (aws-sdk-go-v2) and `pkg/nexoadmin` admin client | T-3.2 | — |
| T-4.3 | `ls`, `mb`, `rb`, `stat`, `cat`, `pipe`, `head` | T-4.2 | testscript e2e |
| T-4.4 | `cp`, `mv`, `rm` with recursion, parallel multipart, progress bar, glob | T-4.3 | Copy of 1000 files + one 2 GiB file |
| T-4.5 | `share download/upload/--post/list/revoke`, `presign`, `anonymous`, shell completion | T-4.3, T-2.9 | Tests TCL-P-01..06 |
| T-4.8 | `nexoctl` Docker image and CLI binary packages | T-0.8 | `docker run nexoctl ls` works |
| T-4.6 | `admin info`, `admin user`, `admin policy` | T-4.2 | testscript e2e |
| T-4.7 | `mirror` (+ `--watch` with fsnotify), `find`, `du`, `tree`, `diff` | T-4.4 | Idempotent mirror |

## M5 — Single-node erasure coding

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-5.1 | Endpoint parsing with ellipses, set size computation, validation | T-0.5 | Table-driven tests on the topologies |
| T-5.2 | `erasure`: block-based streaming encode/decode with `reedsolomon`, per-block HighwayHash bitrot | T-1.1 | Reconstruction with P missing shards |
| T-5.3 | `ErasureObjects`: put/get/delete with quorum, choice of consistent metadata (`pickValidFileInfo`) | T-5.2 | Tests with "naughty" disks (fault injection) |
| T-5.4 | `ErasureSets` + `ServerPools`, placement hash | T-5.3 | Uniform distribution (chi-square) |
| T-5.9 | SNSD/SNMD CI matrix for all the integration suites, `compose.snmd.yml` | T-5.5 | Partial TM-01 green |
| T-5.5 | Listing merge from multiple disks with quorum | T-5.4 | Consistent listing with 1 disk offline |
| T-5.6 | Heal-on-read + MRF + object/bucket heal | T-5.3 | Emptied disk → repopulated |
| T-5.7 | Disk monitor, disk replacement, `healing.bin` with resume | T-5.6 | Healing resumes after restart |
| T-5.8 | Background scanner (usage + sampled heal) with throttling | T-5.6 | Correct usage in `/usage` |

## M6 — Distributed cluster

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-6.1 | Internode RPC framework (HTTP/2, msgpack, streaming, internode JWT, timeouts, reconnect) | T-0.5 | Benchmark ≥ 1 GB/s on loopback |
| T-6.2 | `storageRESTServer/Client` implementing `StorageAPI` remotely | T-6.1, T-5.3 | The same `xlStorage` test suite passes remotely |
| T-6.3 | `dsync` lock server + client with quorum, refresh, expiry | T-6.1 | Tests with nodes crashing while holding locks |
| T-6.4 | Bootstrap: topology check, quorum wait, coordinated formatting | T-6.2 | Nodes started in random order |
| T-6.5 | Peer RPC: IAM/bucket/config cache invalidation, aggregated server info | T-6.1 | A user created on node1 is usable immediately on node3 |
| T-6.6 | Leader election via lock for singleton jobs (scanner, ILM) | T-6.3 | Only one scanner active in the cluster |
| T-6.7 | Expansion with new pools + weighted pool selection | T-5.4 | New objects go to the empty pool |
| T-6.8 | `test/cluster` docker-compose 4×4 environment + toxiproxy, chaos suite | T-6.4 | Chaos suite green in nightly CI |
| T-6.9 | Dashboard Cluster and Healing pages (D-CLU-01, 02), CLI `admin heal` (C-ADM-07) | T-6.5 | E2E on a docker cluster |
| T-6.10 | `compose.cluster.yml` + nginx, `compose.cluster-tls.yml`, public URL (`NEXO_SERVER_URL`) for presigned URLs behind a proxy | T-6.4 | TM-06, TM-09, TPS-19 |
| T-6.11 | Full SNSD/SNMD/Cluster CI matrix (integration, CLI, Playwright) | T-6.10 | TM-01, TM-02 green |
| T-6.12 | `admin cluster export/import` for migration between modes (S-OPS-15, C-ADM-13): IAM, config and bucket metadata in a single archive (today `admin config export/import` covers the configuration only) | T-6.5 | TM-04 |

## M7 — Advanced features

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-7.1 | Versioning, delete markers, ListObjectVersions (S-BKT-04, S-OBJ-10) | M5 | s3-tests versioning green |
| T-7.2 | Bucket/object tagging (S-BKT-05, S-OBJ-11), CORS (S-BKT-06) | M1 | — |
| T-7.3 | Lifecycle: parsing, evaluation in the scanner, expiration/noncurrent/abort MPU (S-BKT-07) | T-5.8, T-7.1 | Objects expire with a simulated clock |
| T-7.4 | Object lock: retention, legal hold, governance bypass (S-BKT-08, S-OBJ-12) | T-7.1 | Delete blocked in compliance mode even for root |
| T-7.5 | Internal KMS (master key from env) + external KMS interface; SSE-S3/SSE-C/SSE-KMS in DARE format (S-OBJ-13, S-BKT-11) | M1 | On-disk data not readable in clear text; range GET on encrypted objects |
| T-7.6 | Event notifications: Webhook, Kafka, NATS, AMQP, Redis, PostgreSQL targets; persistent queue for offline targets (S-BKT-10) | M1 | Event delivered after the target recovers |
| T-7.7 | Bucket quota (S-BKT-09) | T-5.8 | PUT over quota → 507/`XNexoQuotaExceeded` |
| T-7.8 | Additional S3 checksums (S-OBJ-09) | T-1.6 | — |
| T-7.9 | Dashboard: bucket settings, lifecycle, events, object/version detail, preview, groups, access keys (D-BKT-02..04, D-OBJ-05..06, D-IAM-02, 04). Already in M3/M4: bucket settings (versioning, quota, tags, default encryption, object lock with default retention), version restore and deletion, preview, groups, access keys. Left for M7: lifecycle (D-BKT-03) and events (D-BKT-04), currently 501 in the Admin API, retention and legal hold of a single object (D-OBJ-06) | T-7.1..T-7.7 | E2E for every page |
| T-7.10 | CLI: `version`, `tag`, `retention`, `legalhold`, `ilm`, `event`, `quota`, `encrypt`, `anonymous`, `admin group`, `admin accesskey`, `admin config`. Already in M3/M4: `version`, `tag`, `quota`, `encrypt`, `anonymous`, `admin group/accesskey/config`. Left for M7: `retention`, `legalhold`, `ilm`, `event` (with T-7.3, T-7.4, T-7.6) | T-7.1..T-7.7 | testscript for every command |

**S3 compatibility status (DoD of T-7.1, T-9.1 brought forward for `s3-tests`)** — `make test-s3compat`
(`test/compat`, nightly job `s3tests-nightly` in all three modes, [06 §6.1](06-test-plan.md)) on
`ceph/s3-tests@5522d1c` (`test_s3.py`, 838 tests), measured on 2026-10-09:

- 415 tests excluded, each with its reason in `test/compat/excluded.txt`: 235 marked `fails_on_aws` by the suite
  itself (RGW extensions, logging among them), 83 ACL (S-BKT-13, P2), 37 V2 signatures, 9 PublicAccessBlock (S-ACC-03),
  the rest RGW behaviors that differ from S3 or known differences ([02 §2.4](02-features.md));
- 422 applicable: **422/422 in SNMD and cluster, 421/422 in SNSD**; `test_atomic_read_4mb` failed in 2 SNSD runs
  out of 4 (never in SNMD or cluster): the suite's client writes the key while it is not yet reading the GET of the same key,
  and the read lock held during the transfer (as in MinIO) blocks the PUT until the socket buffers
  absorb the object or `NEXO_SEND_TIMEOUT` (20 s) expires. Fixed in T-9.1 (snapshot reads that
  release the lock before the transfer, [02 §2.4](02-features.md)); the current status, on all the files
  of the suite and with `mint`, is in the T-9.1 status (M9, below);
- M7 target groups (90% threshold per group): versioning 28/28, object lock 39/39, multipart 31/31,
  tagging 17/17, SSE-C 18/18, checksum 11/11, CORS 1/1 (13 of the 14 CORS tests use public ACLs or V2 signatures;
  CORS is covered by `TestBucketCORS` in the integration matrix);
- starting point, with only the marker exclusions: 393/592 (66%). The fixes (S3 in general, not
  specific to the suite) are covered by unit tests and by `test/integration/compat_test.go` in the TM-01 matrix.

## M7b — Access, domains and distributions

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-7b.1 | prefix/glob/regex (RE2) matcher with key normalization, limits, compiled cache | T-2.3 | TAR-01..04, TAR-10 |
| T-7b.2 | Access rules in the policy engine (priority, conditions, named groups), stored in bucket metadata, peer propagation | T-7b.1, T-6.5 | TAR-05..09 |
| T-7b.3 | Public presets and block public access | T-7b.2 | TAR-08 |
| T-7b.4 | Trusted proxies, `Forwarded`/`X-Forwarded-*`, PROXY protocol, real IP in conditions | T-0.5 | TDS-08 |
| T-7b.5 | Distribution model + Host routing table (exact, wildcard) updated hot | T-6.5 | TDS-01, 02 |
| T-7b.6 | `website` mode: index, error page, SPA fallback, redirects, headers, CORS, per-path behaviors | T-7b.5 | TDS-03, 05, 09, 12 |
| T-7b.7 | `s3` mode on a custom domain (SigV4/presigned with public host) | T-7b.5 | TDS-04 |
| T-7b.8 | Origin secret for proxies with rotation, IP restrictions | T-7b.4 | TDS-07 |
| T-7b.9 | Key groups, distribution signed URLs and cookies | T-7b.6 | TDS-06 |
| T-7b.10 | Per-domain TLS: SNI, upload, ACME with renewal by the leader in a cluster; `--web-address`/`--web-tls-address` listeners | T-6.6 | TDS-10 |
| T-7b.11 | Admin API + OpenAPI for rules/distributions/certificates/routing resolve | T-7b.2, T-7b.5 | Contract tests |
| T-7b.12 | Dashboard: access rules with tester, presets, block public (D-ACC-01..04) | T-7b.11 | TD-A-01..03 |
| T-7b.13 | Dashboard: distributions, wizard, behaviors with tester, key groups, certificates, origin secret (D-DST-01..07) | T-7b.11 | TD-D-01..05 |
| T-7b.14 | CLI `access` and `dist` | T-7b.11 | TCL-A-*, TCL-D-* |
| T-7b.15 | `deploy/proxy/` (nginx, Caddy, Traefik, HAProxy, Cloudflare) + compose with 2 proxies | T-7b.8 | TCD-01..03 (`make test-proxy`: `test/proxy`, in-process Go proxy in every mode and real nginx/HAProxy with the files of `compose.multi-proxy.yml`) |
| T-7b.16 | Per-distribution metrics/access log (D-DST-08); the cache is in M7c | T-8.1 | — |

## M7c — Per-domain cache

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-7c.1 | Cache policy model + global/distribution/domain/behavior inheritance | T-7b.5 | TCC-01, 02 |
| T-7c.2 | Cache key computation (query, headers, cookies, host, normalization) | T-7c.1 | TCC-03 |
| T-7c.3 | L1 memory cache (W-TinyLFU) + L2 disk cache with index, global/distribution/domain quotas | T-7c.1 | TCC-13, 14, 16 |
| T-7c.4 | Cache middleware: TTL, stale-while-revalidate/if-error, negative caching, bypass, request collapsing, compressed variants, `X-Cache`/`Age` headers | T-7c.2, T-7c.3 | TCC-04, 07, 08, 10, 11 |
| T-7c.5 | Block-based range caching | T-7c.4 | TCC-09 |
| T-7c.6 | Automatic invalidation from write events + manual (path/glob/regex/tag/domain/soft), propagation with sequence number | T-7c.4, T-6.5 | TCC-05, 06, 12, TCC-C-03, 04 |
| T-7c.7 | `shared` cluster mode with rendezvous hashing via RPC | T-7c.4, T-6.1 | TCC-C-01, 02 |
| T-7c.8 | External purge webhooks with persistent queue | T-7c.6 | TCC-15 |
| T-7c.9 | Warm-up (prefetch, warmOnWrite) | T-7c.4 | TCL-C-04 |
| T-7c.10 | URL inspector, per-domain metrics | T-7c.4, T-8.1 | TCL-C-03 |
| T-7c.11 | Admin API + OpenAPI, CLI `cache` and `dist cache` | T-7c.6 | TCL-C-* |
| T-7c.12 | Dashboard D-CCH-01..08 | T-7c.11 | TD-C-* |
| T-7c.13 | `/cache` volume in the Compose files, `--cache-dir/--cache-size` flags, compose test with 2 proxies | T-7c.3 | TCC-C-05 (`make test-proxy`) |

**Status of M7b and M7c (integration of 2026-10-09)**: T-7b.1..T-7b.16 and T-7c.1..T-7c.13 completed in SNSD, SNMD and
cluster. Access rules, block public access, distributions, key groups, certificates and cache configuration
live in the erasure-coded system store and reach the other nodes through the `cluster.Notifier`, without restart (TAR-09:
about 3 ms from node1 to node4; TCD-03: tens of ms through the proxies).

- **A single matcher**: `internal/service/match` (prefixes in a trie, globs translated to RE2, RE2 regexes, NFC normalization
  of `//`, `.` and `..`) evaluates the access rules, the distribution behaviors (service, `dist test` and website
  engine: `distribution.PathPattern` is `match.Pattern`), the per-path settings, the warm-up and the cache
  purges; the Varnish bans of the webhooks use the matcher's RE2 expression (`Matcher.Expr`) and the in-memory Admin of the
  CLI tests uses the same matcher. A single `distribution.NormalizeHost` for the routing table and the cache.
- Measurements: TAR-10 p99 per request 5.2 µs with 200 regexes (limit 50 µs); host resolution 162 ns/op with 11,000
  aliases (9,900 exact, 1,100 wildcard); TCC-16 L1 HIT on 64 KiB p50 4.6 µs, p99 40 µs (limit 1 ms).
- Integration checks (4 CPUs): `go test -race` of the packages 3 min 8 s; integration (TM-01, with the slow
  tests) SNSD 75 s, SNMD 47 s, cluster 246 s; CLI `test/cli` SNSD 199 s, SNMD 34 s, cluster 230 s; `make
  test-cluster` 249 s; `NEXO_PROXY_REAL=1 make test-proxy` (Go proxy in every mode, real nginx and HAProxy) 23 s;
  Vitest 62 files / 660 tests 72 s; Playwright 112 tests (chromium, snmd, cluster) 2.7 min; initial bundle 306.25 kB
  gzip (limit 307.2); `golangci-lint` 0 issues also with the `cluster,compose,proxy` tags; `go generate` and
  `npm run gen:api` idempotent.
- Smoke test with the real binary (`bin/nexo` with the dashboard, SNMD 4 disks): website distribution on
  `www.example.test` and `example.test` (`Host` header), MISS then HIT on both domains, six consecutive PUTs
  never serving stale content on either, regex `deny` rule on `private/` (403 also with `%70rivate` and
  `public/../private`, also for objects already cached), valid signed URL (200), tampered (403) and expired (403).
- Still P2: per-distribution rate limit (TDS-13: validated and stored, not enforced) and ACME DNS-01 challenge for
  wildcard certificates (both brought forward and done in October 2026, see M10).

## M8 — Observability

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-8.1 | Prometheus metrics (API, histogram latencies, disks, network, heal, scanner, ILM) | M6 | Example Grafana dashboard in `deploy/` |
| T-8.2 | Metrics ring buffer for the console charts + `/metrics/query` endpoint | T-8.1 | — |
| T-8.3 | Live trace and live logs aggregated from the cluster via SSE (S-OPS-07) | T-6.5 | `admin trace` shows requests from all the nodes |
| T-8.4 | Audit log + webhook/Kafka targets (S-OPS-06) | T-7.6 | Every authenticated request has an audit record |
| T-8.5 | OpenTelemetry tracing | M1 | Span per request and per RPC |
| T-8.6 | Dashboard Metrics, live Logs/Trace, Configuration (D-MON-01, 02, D-CFG-01); CLI `admin trace/logs` | T-8.2, T-8.3 | E2E |

**Status of T-8.1–T-8.5 (observability backend), completed.** Details in [07 §7.4](07-security-operations.md).
- T-8.1: Prometheus metrics for API (counters and latency histograms per API and bucket), drives, locks, KMS, events,
  lifecycle, healing, scanner, internode RPC, cache and distributions, exposed on `/nexo/metrics/v1/{cluster,node,bucket}`
  (JWT `admin:Prometheus` or public with `NEXO_PROMETHEUS_AUTH_TYPE=public`), aggregated for the cluster by the node
  that receives the scrape; Grafana dashboard `nexo-overview`, alerts and Prometheus configurations in `deploy/docker/observability/`
  (validated by `config_test.go` against the exposed families).
- T-8.2: per-node ring buffer (one point every 10 s for 24 h, lazy growth) and `GET /api/v1/metrics` +
  `/api/v1/metrics/query` with the cluster aggregate.
- T-8.3: live trace and logs via SSE (`/api/v1/trace`, `/api/v1/logs`) aggregated from the nodes with RPC streams, filters,
  secret redaction, counter of lost events without slowing down requests.
- T-8.4: one audit record for every authenticated request (S3 and console) in the JSON log, in memory (search
  `/api/v1/audit`) and to the `audit_webhook`/`audit_kafka` targets with a persistent queue.
- T-8.5: OpenTelemetry OTLP (`http/protobuf` or `grpc`), one server span per S3/console/site/RPC request and one client
  span per internode call with `traceparent` propagation; zero cost when disabled.
- Integration: `TestLiveTrace`, `TestAuditLog`, `TestClusterMetrics` in the SNSD/SNMD/cluster matrix. The audit and log
  rings grow on demand and the audit target dispatcher starts only with a configured target: heap of
  8 clusters of 4 nodes from 328 to 122 MB (the integration suite with `-race` no longer runs out of memory).

**Status of T-8.6.**
- Dashboard: Monitoring page (`/monitoring/{metrics,trace,logs,audit}`, lazy, sections visible according to
  `admin:Prometheus`/`admin:ServerInfo`, `admin:ServerTrace`, `admin:ConsoleLog`): charts from `/metrics/query`
  (requests, errors, p50/p95/p99 latencies, bandwidth, disks, healing/scanner, internode, CPU, memory; cluster or node,
  time range, auto-refresh, node comparison), live trace and logs via SSE (filters, pause/resume, bounded buffer,
  reconnection), audit search; Configuration (logger, audit targets, OTel, TLS) and Info with TLS/OTel status.
  Vitest (TD-U-11, TD-U-12) and Playwright `e2e/monitoring.spec.ts` (TD-E-16, metrics, audit, permissions, axe).
- CLI (C-ADM-08, C-ADM-12): `nexoctl admin trace|logs` (stream until Ctrl-C, `--timeout`, `--count`,
  reconnection), `admin audit`, `admin metrics list|query`, `admin prometheus generate` (HS512 JWT with
  `iss=prometheus`), `admin speedtest`, `admin profile --seconds`; testscript `test/cli/admin_{trace,observability,
  prometheus,speedtest}.txtar` and `cluster_admin_trace.txtar` (trace of all the nodes, DoD of T-8.3).

## M9 — Hardening and v1.0 release

| ID | Task | Depends on | DoD |
|----|------|------------|-----|
| T-9.1 | Ceph `s3-tests` and MinIO `mint` compatibility suites in nightly CI. `s3-tests` brought forward to M7 (see the M7 status), completed with all the files of the suite, the 95% threshold on every group and `mint` (status below) | M7 | ≥ 95% of the applicable tests green, justified exclusion list |
| T-9.2 | Benchmarks with `warp` and profiling (pprof), optimizations (buffer pools, O_DIRECT, zero-copy) | M6 | Performance report in `docs/bench/` |
| T-9.3 | Security review: threat model, `gosec`, `govulncheck`, `npm audit`, extended fuzzing, console pen test | M8 | No open High vulnerability |
| T-9.4 | TLS hot reload, multiple SNI (S-OPS-02) | M1 | — |
| T-9.5 | Final packaging: multi-arch binaries, deb/rpm/Homebrew, signed Docker images (cosign), SBOM, Helm chart (`standalone`/`distributed`), systemd unit, Swarm stack | M8 | Release candidate published, TM-10, TM-11 |
| T-9.6 | User documentation: quickstart, cluster guide, operations guide, CLI reference generated by cobra | — | Docs site |
| T-9.7 | Complete dashboard accessibility and i18n (D-UX-01) | M8 | axe without serious/critical violations |
| T-9.8 | Upgrade test (v1.0-rc → v1.0) with existing data, rolling restart. Before the first release: from the M7 release (`f70cc7a`) to the working tree (status below) | T-9.5 | No data loss |

**Status of T-9.2, T-9.3, T-9.4 (hardening).**
- T-9.2: `warp` v1.8.2 benchmarks in SNSD, SNMD and a 4-process cluster (`make bench`, `test/bench/warp.sh`),
  profiling via the Admin API (`POST /api/v1/profile`, `nexoctl admin profile`) and speed test (`POST /api/v1/speedtest/*`,
  `nexoctl admin speedtest`, S-OPS-11, brought forward from T-10.7); measured optimizations (erasure coding buffers
  recycled and sized to the part, drive write buffers recycled): report in
  [docs/bench/README.md](bench/README.md). O_DIRECT and zero-copy (October 2026, [§7](bench/README.md#7-o_direct-and-zero-copy-t-92-october-2026)):
  single-drive downloads without SSE go from the page cache to the socket with `sendfile` (−47 to −66% server CPU
  per GiB); O_DIRECT for large data files was measured slower than page cache + fsync (−30% on 4 MiB writes) and
  left out.
- T-9.3: STRIDE threat model per surface, `make security` (govulncheck, gosec, npm audit) and `security` CI job,
  24 fuzzing targets (`make fuzz`, nightly job `fuzz-nightly`): [docs/security/threat-model.md](security/threat-model.md).
  With v1.0 the toolchain is Go 1.26.9 (`go.mod`, Dockerfile, CI with golangci-lint v2.14.0) and the vulnerable modules
  are updated (`x/net` v0.60.0, `x/text` v0.42.0, pgx v5.9.2, OpenTelemetry v1.45.0, grpc v1.83.2,
  `klauspost/compress` v1.18.7, chi v5.3.0): **`govulncheck` with no reachable vulnerabilities** (GO-2026-5932 in
  `x/crypto` remains, with no fix and not reachable), §10.1 of the threat model. The internode RPC client uses the HTTP/2 of `net/http`
  (`http.Transport` with `Protocols`), no longer the deprecated `x/net/http2`. The console pen test is missing.
- T-9.4: `--certs-dir` keystore with SNI, hot reload (fsnotify and SIGHUP) without interrupting connections,
  TLS ≥ 1.2 (`--tls-min-version`), chain with the distribution certificates ([docs/07 §7.3](07-security-operations.md#73-tls)).

**Status of T-9.5 (packaging).** Configuration complete and validated locally, no release published (the
"release candidate published" of the DoD requires the first `v1.0.0-rc.1` tag): [docs/08 §8.7](08-deployment-modes.md#87-release-t-95).
- GoReleaser v2.18.3 (`goreleaser check` without deprecations; `make release-snapshot` builds the 11 binaries
  with the console, 11 archives + sources, 8 `nexo`/`nexoctl` deb/rpm packages, checksums, 11 SPDX SBOMs, Homebrew
  cask); with a Docker daemon `make release-snapshot RELEASE_SKIP=sign` also builds the
  linux/amd64 and linux/arm64 images (`dockers_v2`), checked by the `packaging` CI job (architecture, UID 65532,
  `version`, HEALTHCHECK with TLS). Push, SBOM/provenance attestations, keyless cosign signatures (Sigstore
  bundle) and OCI chart only in the `release.yml` workflow.
- TM-10: `packaging/smoke.sh` (version = tag, `--dry-run`, server, console, S3 round trip, SIGTERM) green on
  linux/amd64 and linux/arm64 with QEMU; release `smoke` job on darwin and windows. TM-11: `verify` job
  (checksum and image signatures, SBOM attestations, trivy).
- Helm chart `deploy/helm/nexo` (`standalone`/`distributed`, `helm test`), Swarm stack `deploy/swarm`, systemd
  unit with sandboxing (exposure 1.5), `packaging` CI job (snapshot, installed packages, chart, unit,
  Dockerfile, image `HEALTHCHECK` with TLS).
- `nexo healthcheck` (HEALTHCHECK of the images) follows `NEXO_CERTS_DIR`/`--certs-dir`: HTTPS to the
  console, trust in `public.crt` and `CAs/`; previously a container with TLS stayed `unhealthy` (HTTP probe
  against a TLS listener) and Swarm kept replacing its tasks. Outcome of the local checks (validated vs.
  only configured) in [docs/08 §8.7](08-deployment-modes.md#validated-locally-and-only-configured).

**Status of T-9.1 (compatibility), measured on 2026-10-15** ([06 §6.1](06-test-plan.md)).
- `ceph/s3-tests@5522d1c`, all the files of the suite (`test_s3.py`, `test_headers.py`, `test_iam.py`, `test_sts.py`,
  `test_s3select.py`, `test_sns.py`, `test_s3control.py`): 1052 tests collected, 598 excluded with a reason
  (`test/compat/excluded.txt`: 265 `rgw`, 146 `p2`, 91 `out-of-catalog`, 47 `deprecated`, 32 `aws`, 11 `design`,
  6 `suite`), 3 skipped by the suite itself, **451 applicable: 451/451 in SNMD, SNSD and cluster**; all 17
  groups with applicable tests at 100% (95% threshold), among them object 110, listing 79, object-lock 39, sse-s3-kms 34,
  multipart 31, versioning 28, headers 18, sse-c 18, tagging 17, post-object 12, checksum 10. Review of the
  exclusions: the 168 tests excluded by name, when run (`NEXO_S3TESTS_CHECK_EXCLUDED=names`, also in the nightly
  job), all fail.
- `minio/mint@12559d5` without Docker (`make test-mint`, nightly job `mint-nightly`), all 14 suites built
  and run, identical in SNMD and cluster: **638 tests passed, 0 unexpected failures**, 106 expected failures
  (`test/compat/mint-excluded.txt`: 67 `minio`, MinIO extensions and Admin API; 22 `deprecated`, SigV2 of
  minio-go; 5 `p2`, S3 Select; 4 `out-of-catalog`, MD5 checksum; 4 `aws`; 3 `design`; 1 `suite`), 18 NA declared
  by the suites. Per suite: minio-go 143, minio-js 239, minio-java 109, mc 39, awscli 21, minio-py 18,
  aws-sdk-go-v2 19, versioning 13, aws-sdk-php 11, aws-sdk-ruby 11, s3cmd 8, aws-sdk-java-v2 7; healthcheck
  (MinIO endpoint) and s3select (P2) without applicable tests. The suites that stop at the first error
  (aws-sdk-go-v2, aws-sdk-php, aws-sdk-ruby, mc, minio-py, versioning) do not run the tests that follow an
  expected failure.
- Root-cause fixes that came out of the two suites (unit tests and `test/integration/compat_test.go` in the matrix):
  snapshot reads (`test_atomic_read_4mb`), conditional `DeleteObject` with `If-Match` (412; 501 for the
  conditions of directory buckets only), `NoSuchVersion` for a `versionId` of a nonexistent key,
  `PutBucketTagging` 200, `NextPartNumberMarker` on the last page of `ListParts`/`GetObjectAttributes`,
  the grantee `Type` element in ACLs, malformed `x-amz-content-sha256` in presigned URLs, unnamed file field
  in POSTs (`MalformedPOSTRequest`) and checksum in the response, CORS (lower-case headers, non-preflight requests
  with `Access-Control-Request-Method`), `POST` on an object without a subresource 405, unsupported checksum
  algorithms rejected, unencoded quotes in XML error messages ([02 §2.4](02-features.md)).

**Status of T-9.8 (upgrade), measured on 2026-10-15.** No release has been published yet: the previous
release is the M7 commit `f70cc7a` (built as `v0.7.0`), the release under test is the working tree (`v0.9.0`);
`make test-upgrade` (nightly job `upgrade-nightly`) green in 46 s.
- `TestUpgradeSNMD`: 14 versions of 10 keys written by `v0.7.0` (versions and delete markers, CRC32C multipart and
  an open upload, SSE-S3/KMS/C, default encryption, Object Lock, tags, IAM, bucket policy, lifecycle, CORS,
  notifications with a queued event) read back by `v0.9.0` (sha256 of every version), queued event delivered,
  data modified (18 versions); formats unchanged (`format.json` v1, `xl.meta` 1.x), hence downgrade verified:
  `v0.7.0` serves the disks modified by the new release and writes to them, `v0.9.0` reads everything back (21 versions of 15 keys).
- `TestRollingUpgradeCluster`: 4 nodes × 2 disks upgraded one at a time (1.6–2.7 s per node) under load
  (1326 operations from 8 workers): 0 client errors beyond the SDK retries, 0 inconsistent reads; rollback of one
  node to `v0.7.0` in the upgraded cluster and back to `v0.9.0` without data loss.
- Downgrade policy: [08, upgrades and downgrades](08-deployment-modes.md).

**Status of T-9.7 (accessibility and i18n).** `@axe-core/playwright` on every page and section of the console
(TD-E-19: `a11y.spec.ts`, `access-distributions-a11y.spec.ts`, cache and monitoring sections) without
serious/critical violations, also in dark theme; it/en without missing keys (`i18n/parity.test.ts`, TD-U-13); dark theme,
English and 375 px viewport (TD-E-18). The contrast of the status badges was darkened in `web/src/theme.ts`.

**Traceability (docs/06 §6.8).** `test/TRACEABILITY.md`, generated by `make traceability` (`cmd/tools/traceability`,
logic and tests in `internal/tools/traceability`, outside the release binaries) from the `covers: <code>` tags of the Go tests,
the `testscript` scripts, the Vitest/Playwright tests and the release scripts: **75/75 P0 features and 73/73 P1
with at least one test**; the 17 P2 features (M10) are listed as "not in v1.0". `make traceability-check` (`lint-go` job and
release workflow) and the `TestRepositoryTraceability` test fail for a P0/P1 feature without tests, for a code outside the
catalog or for a stale matrix. Four P2 features delivered early became P1 in the catalog (S-OPS-11,
D-MON-03, C-ADM-12, S-OPS-10, [02](02-features.md)); the rate limit part of S-DST-08 too.

**Final v1.0 verification (integration, 2026-10-15).** Outcomes and durations in the [CHANGELOG](../CHANGELOG.md#verification);
all green in SNSD, SNMD and cluster. Still open, outside the P0/P1 scope: console pen test (T-9.3),
O_DIRECT and zero-copy (T-9.2), publication of the first release candidate `v1.0.0-rc.1` with signatures and images (T-9.5,
requires the tag), coordinated cluster restart and orchestrated rolling upgrade (S-OPS-09). Per-API and
per-distribution rate limiting with the enforcement of `api.requests_max`/`api.requests_deadline` (S-OPS-10, S-DST-08)
was brought forward from M10 (T-10.7) in October 2026: per-node token buckets, see the [CHANGELOG](../CHANGELOG.md).
The ACME DNS-01 challenge for the wildcard certificates of the distributions (S-DST-06, TDS-10) was brought
forward from M10 in October 2026 too: DNS providers Cloudflare, Route 53 and RFC 2136 with sealed credentials,
propagation check on the authoritative nameservers.

## M10 — Post v1.0 (P2)

| ID | Task |
|----|------|
| T-10.1 | Active-active bucket replication + resync (S-BKT-12) |
| T-10.2 | Site replication (S-STO-11), CLI `admin replicate`, dashboard page |
| T-10.3 | Pool decommission (S-STO-09) and rebalance (S-STO-10) |
| T-10.4 | OIDC (SSO console login + AssumeRoleWithWebIdentity) and LDAP (S-IAM-08, 09) |
| T-10.5 | ILM tiering to remote S3 (S-STO-12) |
| T-10.6 | SelectObjectContent, transparent compression (GetObjectAttributes brought forward to M7) |
| T-10.7 | Orchestrated rolling upgrade (`admin update`); rate limiting (S-OPS-10, S-DST-08) and speedtest (S-OPS-11) brought forward to v1.0 |
| T-10.8 | Kubernetes operator |
