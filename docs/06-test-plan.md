# 6. Test plan

## 6.1 Strategy (pyramid)

| Level | Server (Go) | CLI (Go) | Dashboard (React) |
|-------|-------------|----------|-------------------|
| Unit | `go test -race`, table-driven, mocks of `StorageAPI`/`ObjectLayer` | Command tests with a fake client, golden files of the output | Vitest + React Testing Library + MSW |
| Fuzz | `go test -fuzz` on the XML, SigV4, policy, `xl.meta`, ellipsis endpoint parsers | Path/alias parsers | — |
| Contract | Admin API responses validated against `openapi.yaml` | `nexoadmin` client vs OpenAPI | Generated TS client: typecheck in CI |
| Integration | In-process server + `aws-sdk-go-v2`, `boto3`, `aws-cli` | `testscript` (`.txtar`) against an in-process server | Playwright against the real `nexo` binary with embedded UI |
| Compatibility | Ceph `s3-tests`, MinIO `mint` | — | — |
| Cluster/Chaos | docker-compose 4×4 + toxiproxy + container kill/pause | CLI against a cluster | Cluster/Healing pages against a cluster |
| Performance | `go test -bench`, `warp`, k6 | `cp`/`mirror` throughput | Lighthouse, bundle size budget |
| Security | `gosec`, `govulncheck`, ZAP on the console, negative authz tests | Config file permissions | CSP, XSS, CSRF |

### CI pipeline

| Trigger | Jobs |
|---------|------|
| Every PR | Go/TS lint, Go unit (`-race`, coverage), web unit, build with embedded UI, server integration, CLI testscript, Playwright smoke (Chromium), OpenAPI contract |
| Merge to `main` | + full Playwright (Chromium, Firefox, WebKit), 5 min fuzzing per target, 4-node cluster smoke |
| Nightly | s3-tests, mint, upgrade tests, cluster chaos suite, 1 h fuzzing, benchmarks compared against the baseline (regression > 10% = failure) |
| Release | Upgrade test from the previous release, image scanning (trivy), SBOM |

### Ceph `s3-tests` compatibility suite

`test/compat/run-s3tests.sh` (`make test-s3compat`, nightly job `s3tests-nightly`) downloads `ceph/s3-tests` at a
pinned commit, creates a Python venv with the dependencies pinned by `test/compat/constraints.txt`, starts `bin/nexo`
over TLS with the built-in KMS (4-disk SNMD; `NEXO_S3TESTS_MODE=snsd|cluster` for the other modes, all three in the
nightly job; the deployment is the one of `test/compat/lib.sh`, shared with `mint`), creates with `nexoctl admin` the
suite's `main`, `alt` and `tenant` users and the KMS keys, generates `s3tests.conf` and runs with pytest all the files
of the suite (`test_s3.py`, `test_headers.py`, `test_iam.py`, `test_sts.py`, `test_s3select.py`, `test_sns.py`,
`test_s3control.py`; `NEXO_S3TESTS_FILES` to narrow them). The `test/compat/nexo_s3tests.py` plugin deselects
the tests of `test/compat/excluded.txt` (`<selector> | <category> | <reason>`, categories: `rgw` RGW extensions or
expectations that AWS itself does not meet, `deprecated` deprecated APIs such as SigV2, `p2` P2 features of the
catalog, `out-of-catalog`, `aws` Nexo's response equal to AWS's and different from the expected one, `design`
choices of [02 §2.4](02-features.md), `suite` defects of the suite), classifies the tests by functional group and
writes `report.txt`/`report.json` (outcome per test, per group, exclusions per category). The job fails if a
group with applicable tests passes less than 95% of them (`NEXO_S3TESTS_THRESHOLD`). With
`NEXO_S3TESTS_CHECK_EXCLUDED=names` (in the nightly job) it also runs the tests excluded by name and fails if one
passes: the exclusion must then be removed. Without network access (git/pip) the local run is skipped and documented in
`report/SKIPPED`; in CI (`NEXO_S3TESTS_REQUIRE_NETWORK=1`) it is an error. The measured status is in
[05, T-9.1 status](05-roadmap.md).

### MinIO `mint` compatibility suite

`test/compat/run-mint.sh` (`make test-mint`, nightly job `mint-nightly` in SNMD and cluster) runs the SDK suites of
`minio/mint` at a pinned commit **without Docker**: it builds every suite on the host with the language toolchain
(Go: minio-go, aws-sdk-go-v2, versioning, healthcheck, mc; Python: minio-py, awscli, s3cmd, s3select; Node.js:
minio-js; Java: minio-java, aws-sdk-java-v2 with Gradle; Ruby: aws-sdk-ruby; PHP: aws-sdk-php with Composer), with
the SDK versions pinned in the script, once per version; it starts `bin/nexo` (`lib.sh`, TLS, built-in KMS;
`NEXO_MINT_MODE=snmd|snsd|cluster`) and launches every suite with the environment of `mint.sh` (`MINT_MODE=full`,
test certificate among the trusted CAs of every runtime). `test/compat/mint_report.py` reads the JSON log of every
suite and writes `report.txt`/`report.json` per suite (passed, failed, expected, NA); the expected failures are in
`test/compat/mint-excluded.txt` (`<suite> | <glob on the test name or on "<function> <arguments>"> | <category>
| <reason>`, the categories of `excluded.txt` plus `minio`, MinIO-specific extensions and responses). The script
fails if a suite fails a test that is not listed or cannot be built (`NEXO_MINT_REQUIRE_ALL=1` in CI).
Many suites stop at the first error: an expected failure hides the tests that follow it (noted in
`mint-excluded.txt`).

### Upgrade tests (T-9.8)

`test/upgrade` (build tag `upgrade`, `make test-upgrade`, nightly job `upgrade-nightly`): `TestMain` builds
the previous release from its commit (`NEXO_UPGRADE_FROM`, default `224bb31`, M7, extracted with `git archive`) and the
release under test from the working tree (`NEXO_UPGRADE_OLD_BIN`/`NEXO_UPGRADE_NEW_BIN` for prebuilt binaries); the
deployments use TLS and the built-in KMS.

- `TestUpgradeSNMD` (4 disks): the previous release writes objects with multiple versions and delete markers, multipart
  with CRC32C checksums and an upload left open, SSE-S3, SSE-KMS, SSE-C, default encryption, Object Lock
  (governance retention, legal hold), tags and metadata, IAM users, policies and access keys, anonymous bucket policy,
  lifecycle, CORS, notifications to a webhook and an event queued while the webhook is unreachable. The release under test
  reads everything back (sha256 of every version, ETag, checksums, decryption, locks, configurations, permissions), delivers
  the queued event, completes the open upload and modifies the data; then the previous release restarts on the same
  disks (downgrade) and finally the release under test again.
- `TestRollingUpgradeCluster` (4 nodes × 2 disks behind a load balancer with health checks): one node upgraded
  at a time (SIGTERM, start of the new release, wait for quorum) while 8 workers write, read
  (compared with the sha256 of the last confirmed write) and delete, and a multipart upload receives a part
  while a node is down: no client errors beyond the SDK retries, no inconsistent reads, all the nodes online with
  the new version, data of the previous release intact. Then one node goes back to the previous release (rollback
  of one node), serves the data written by the new one, writes new data read by the upgraded nodes, and restarts with the
  new one.

Downgrade policy ([08](08-deployment-modes.md)): a release reads the disks of the previous releases of the
same major version. Going back is supported only between releases with the same formats (`format.json` version,
`xl.meta` major, internode protocol): the tests verify that the release under test still writes the formats that
the previous one reads and in that case that the downgrade works; if a format changes, the previous release must
reject the disks (not start) leaving them intact, and the tests verify that instead of the downgrade.

**Coverage thresholds**: `internal/{auth,policy,erasure,storage,object,iam}` ≥ 85%; rest of the backend ≥ 70%; web ≥ 75% (statements).

---

## 6.2 Server tests

### Unit
| ID | Area | Cases |
|----|------|-------|
| TS-U-01 | Endpoints/ellipses | `{1...4}`, `{a...d}`, zero-padding `{01...16}`, combinations, invalid input, duplicate disks, http/https mix → error |
| TS-U-02 | Set computation | For 1–32 nodes × 1–32 disks: valid set size, symmetry across nodes, default parity |
| TS-U-03 | Erasure | Encode/decode for every (D,P); reconstruction with 1..P missing shards; error with P+1; partial blocks; objects of 0 bytes, 1 byte, block size ±1 |
| TS-U-04 | Bitrot | 1-bit corruption detected; per-block hash; streaming verification |
| TS-U-05 | `xl.meta` | Round trip, multiple versions, inline data, compatibility with previous versions of the format (binary fixtures), fuzz |
| TS-U-06 | Metadata quorum | Choice of the correct version with diverging disks (different mod-time, data-dir), corrupted metadata |
| TS-U-07 | SigV4 | Official AWS vectors; presigned expiry (`X-Amz-Expires` max 7 days); clock skew > 15 min; missing signed headers; chunked with a wrong chunk signature; checksum trailer |
| TS-U-08 | Policy engine | Allow/Deny, explicit deny wins, wildcards in Action/Resource, every Condition operator, variables, NotAction/NotResource, invalid policies rejected |
| TS-U-09 | Bucket/object names | Bucket DNS rules; keys with unicode, `//`, `..`, `\`, null byte, 1024-byte length; path traversal impossible |
| TS-U-10 | S3 errors | Every `ErrorCode` → correct HTTP status and XML |
| TS-U-11 | Lifecycle | Rule evaluation with a fake clock: prefix/tag/size filters, days, dates, noncurrent (including the `null` version written before versioning), MPU; deletions are conditional on the evaluated version (a write interleaved between evaluation and deletion is never removed or hidden) |
| TS-U-12 | Object lock | Matrix governance/compliance × bypass × legal hold × user |
| TS-U-13 | Crypto | DARE encrypt/decrypt, ranges on encrypted objects, master key rotation with resealing of the data keys (`admin kms reseal`), distinct keys for every upload of a part, sealed checksums, SSE-C wrong key → 403; anti-truncation (a failing source, even with `io.ErrUnexpectedEOF`, fails the stream and the erasure encoding: a truncated body of unknown size is `400 IncompleteBody` in every mode, with and without SSE); in-progress multipart uploads resealed by `reseal`; keys redacted in logs and JSON; in the matrix also with a simulated KES over mTLS (`testutil.WithKES`) and SSE-C rejected over plain HTTP |
| TS-U-14 | dsync | Acquisition with quorum, quorum loss during refresh, expiry of orphan locks, concurrent read/write |
| TS-U-15 | Placement | Uniform distribution of keys across sets (chi-square test), hash stability across versions |

### Integration (in-process server, real SDKs)
| ID | Scenario | Expected |
|----|----------|----------|
| TS-I-01 | Bucket cycle: create, list, head, delete (empty / not empty) | 200/409 `BucketNotEmpty` |
| TS-I-02 | Put/Get of objects of 0 B, 1 KiB, 127 KiB (inline), 129 KiB, 100 MiB | Identical content and ETag |
| TS-I-03 | Range GET: `bytes=0-0`, `-500`, `100-`, past the end | Correct 206 / 416 |
| TS-I-04 | Conditional GET/PUT (`If-None-Match: *` on PUT) | 304/412 |
| TS-I-05 | Listing: 10k objects with nested prefix/delimiter, pagination 1/1000, `start-after`, encoding-type=url | No duplicates/omissions |
| TS-I-06 | Multipart: out-of-order parts, repeated part, non-final part < 5 MiB → error, abort, paginated ListParts, UploadPartCopy | S3-compliant |
| TS-I-07 | Presigned URL GET/PUT with curl; expired URL | 200 / 403 |
| TS-I-08 | POST policy from an HTML form with conditions (content-length-range, starts-with) | 204/403 |
| TS-I-09 | readonly/writeonly/custom user: matrix of 20 operations × policy | Expected Allow/Deny |
| TS-I-10 | Public-read bucket, anonymous access to list/get/put | Only what is granted |
| TS-I-11 | Service account with an inline policy more restrictive than the parent | Intersection honored |
| TS-I-12 | Versioning: put×3, delete (marker), get specific version, delete version, suspend; bucket versioned after the first writes (addressable `null` version, deletion of all the versions and forced deletion from the console with no leftover markers) | S3-compliant |
| TS-I-13 | Concurrency: 100 goroutines PUT on the same key + GET | Always a complete, consistent object |
| TS-I-14 | Read-after-write: PUT then immediate GET/LIST | Always visible |
| TS-I-15 | Events: a test webhook receives `ObjectCreated`/`ObjectRemoved` with the standard S3 payload; offline target → delivery after recovery; `sequencer` monotonic per key with skewed clocks; no events for retention/legal hold; webhooks do not follow redirects | — |
| TS-I-16 | Quota: exceeded | Error and no data written |
| TS-I-17 | Server restart: all data, IAM, config, in-progress multipart preserved | — |
| TS-I-18 | Crash (SIGKILL) during a 1 GiB PUT | No partial object; tmp cleaned |
| TS-I-19 | Client compatibility: aws-cli, boto3, rclone, s3cmd, Terraform S3 backend provider | Smoke scripts green |
| TS-I-20 | Admin API: every endpoint with authorized/unauthorized user, invalid input → `problem+json` | Contract honored |

### Single-node erasure (multi-drive)
| ID | Scenario | Expected |
|----|----------|----------|
| TS-E-01 | 4 disks, 1 disk removed (directory renamed) | Read/write OK |
| TS-E-02 | 4 disks EC:2, 2 disks removed | Read OK, write OK only with quorum |
| TS-E-03 | 3 disks out of 4 removed | `503`, no corruption; everything OK after recovery |
| TS-E-04 | Corrupted shard (bit flip) | Correct GET, shard repaired, bitrot metric incremented |
| TS-E-05 | Disk emptied and reinserted | Full healing; resume after a restart mid-healing |
| TS-E-06 | Slow disk (latency fault injection) | p99 latency contained thanks to reads from faster shards |
| TS-E-07 | Full disk | `XNexoStorageFull` error, no partial object |

## 6.3 Cluster tests (docker-compose 4 nodes × 4 disks + toxiproxy)

| ID | Scenario | Expected |
|----|----------|----------|
| TC-01 | Nodes started in random order with delays up to 60 s | Cluster ready once there is quorum |
| TC-02 | Startup with a different topology on one node | The node refuses to start with a clear error |
| TC-03 | Round-robin load balancer: PUT on node1, GET on node3 | Immediate consistency |
| TC-04 | User/policy created on node2 | Valid immediately on all the nodes |
| TC-05 | Stop of 1 node during a `warp` mixed load | 0 client-side errors (with SDK retries), then automatic heal |
| TC-06 | Stop of 2 nodes out of 4 | Reads OK, writes rejected without corruption (quorum) |
| TC-07 | 2+2 network partition (toxiproxy) | No split-brain: no write accepted by both halves |
| TC-08 | 200 ms latency + 5% packet loss on one node | Operations completed within the timeout, no stuck lock |
| TC-09 | A node crashes while holding a write lock | Lock released by its expiry, other writes resume |
| TC-10 | Full replacement of a node's disks | Healing rebuilds everything; checksums of all the objects equal to the original ones |
| TC-11 | Expansion: a second pool added with a rolling restart | No visible downtime; new objects in the new pool; old ones readable |
| TC-12 | Rolling binary upgrade N → N+1 (same source, different versions; the real previous release in `test/upgrade`, §6.1) | No client errors |
| TC-13 | 5 s clock skew on one node | Warning in the logs/console; no functional anomaly |
| TC-14 | Singleton jobs (scanner/ILM) | Exactly one active instance; failover if the leader dies |
| TC-15 | 24 h soak test with mixed load + random chaos (Jepsen-like, verified with a linearizability checker on the keys) | No consistency violation, no memory/goroutine leak |

Data verification: every chaos test uses a **deterministic generator** (seed) and a checker that compares the final state
(key → content/version hash) with the log of the confirmed operations.

## 6.4 CLI tests (`nexoctl`)

Main tool: `github.com/rogpeppe/go-internal/testscript` with `.txtar` files in `test/cli/`,
in-process Nexo server started by the setup, isolated `HOME`.

### Unit
| ID | Case |
|----|------|
| TCL-U-01 | Parsing of `alias/bucket/prefix` targets, Windows/Unix local paths, globs |
| TCL-U-02 | Alias config: creation with 0600 permissions, corrupted file → clear error, config version migration |
| TCL-U-03 | Output formatting: table, `--json` (valid JSON Lines), `--no-color`, human-readable sizes |
| TCL-U-04 | Exit codes: 0 ok, 1 generic error, 2 wrong usage, 3 not found, 4 access denied |
| TCL-U-05 | Help and usage: golden file for every command (detects unwanted changes) |
| TCL-U-06 | Multipart part size computation for files from 1 B to 5 TiB (max 10000 parts) |

### End-to-end (testscript)
| ID | Scenario | Expected |
|----|----------|----------|
| TCL-E-01 | `alias set` with wrong credentials | Authentication error, exit 4 |
| TCL-E-02 | `mb`, `ls`, `rb` on an empty and a non-empty bucket (`--force`) | Correct output and state |
| TCL-E-03 | `cp` single file, recursive, remote→remote, stdin→remote (`pipe`), with metadata and tags | Identical hashes |
| TCL-E-04 | `cp` of 2 GiB interrupted (SIGINT) | Clean multipart abort, no pending upload |
| TCL-E-05 | `mv`, `rm -r --force`, `rm --versions` | — |
| TCL-E-06 | `mirror` twice | Second run: 0 transfers |
| TCL-E-07 | `mirror --remove` and `--watch` (file created/modified/deleted) | Synchronization within 2 s |
| TCL-E-08 | `share download --expire 1h` | URL downloadable with curl |
| TCL-E-09 | `stat`, `cat`, `head -n`, `find --name "*.log" --older-than 1d`, `du`, `tree` | Expected output |
| TCL-E-10 | `admin user add/list/disable/remove`, login with a disabled user | Access denied |
| TCL-E-11 | `admin policy create` from an invalid JSON file | Error with line/column |
| TCL-E-12 | `admin policy attach` + check of the effective permissions with operations | — |
| TCL-E-13 | `admin accesskey create --expiry` | Key expires |
| TCL-E-14 | `admin config set/get/export/import` | Identical round trip |
| TCL-E-15 | `admin info` on a cluster with one node offline | Node shown offline, exit 0 |
| TCL-E-16 | `admin heal -r --dry-run` | Report without changes |
| TCL-E-17 | `admin trace` receives the requests generated by another process | — |
| TCL-E-18 | All the commands with `--json` | Output valid against the JSON schema |
| TCL-E-19 | bash/zsh completion generated and working on aliases/buckets | — |
| TCL-E-20 | Cross-platform: suite run on Linux, macOS, Windows in CI | — |

## 6.5 Dashboard tests (React)

### Unit / components (Vitest + RTL + MSW)
| ID | Component | Cases |
|----|-----------|-------|
| TD-U-01 | Login form | Field validation, 401 error shown, disabled during submit, post-login redirect to the requested page |
| TD-U-02 | Auth guard | Expired session (401 from any query) → redirect to login with a message |
| TD-U-03 | Bucket table | Sorting, search, empty state, loading skeleton, error with retry |
| TD-U-04 | Create bucket dialog | Live DNS name validation, versioning/lock/quota options, `BucketAlreadyExists` error |
| TD-U-05 | Object browser | Breadcrumb from prefix, folder navigation, multiple selection, cursor pagination, keys with special characters (spaces, `#`, `%`, unicode) |
| TD-U-06 | Upload manager | Part computation, max concurrency, retry of a failed part, pause/resume, cancel (calls abort), aggregated progress |
| TD-U-07 | Policy editor | Invalid JSON highlighted, validation errors from the server shown, policy builder ↔ JSON kept in sync |
| TD-U-08 | Lifecycle form | Form ↔ XML/JSON rule conversion, validation (days > 0, filter) |
| TD-U-09 | Permission-aware UI | With readonly permissions the create/delete buttons are missing; disallowed actions disabled with a tooltip |
| TD-U-10 | Share dialog | Max expiry 7 days, copy to clipboard |
| TD-U-11 | Metrics charts | Rendering with empty data, multiple series, unit formatting |
| TD-U-12 | Live logs | SSE reconnection after a disconnection, pause/resume, line buffer limit |
| TD-U-13 | i18n | No missing keys in it/en (test comparing the files) |
| TD-U-14 | API hooks | `problem+json` error handling, TanStack Query cache invalidation after mutations |

### End-to-end (Playwright against the real `bin/nexo` with embedded UI)
Setup: the global setup starts `nexo server` on a temporary directory (and, for the cluster suite, the docker-compose), creates test users via the Admin API.

| ID | Flow | Expected |
|----|------|----------|
| TD-E-01 | Root login → overview → logout | Cookie removed, access to a protected page → login |
| TD-E-02 | Login with wrong credentials ×5 | Error message, rate limiting after N attempts |
| TD-E-03 | Deep link `/buckets/foo/browse/a/b/` after login | Correct page (verifies the Go-side SPA fallback) |
| TD-E-04 | Create a bucket, check it in the list, delete it | — |
| TD-E-05 | Drag&drop upload of a folder with 50 nested files | Correct prefix structure |
| TD-E-06 | Upload of a 200 MiB file, cancelled halfway | No pending multipart upload on the server |
| TD-E-07 | Single download and zip of 3 objects | Files downloaded with the expected content |
| TD-E-08 | Create a folder, move an object, delete a prefix recursively | — |
| TD-E-09 | Share link: opened in an anonymous context | Download OK; 403 after expiry |
| TD-E-10 | Preview of image, PDF, text | Rendering |
| TD-E-11 | Versioning: enable, upload 3 versions, restore the first, delete a version | — |
| TD-E-12 | Create a custom policy (bucket `x` only), create a user, assign it; log in with that user | Sees only `x`, does not see the Admin menu |
| TD-E-13 | Create an access key with expiry | Secret shown only once; works with the SDK |
| TD-E-14 | Lifecycle rule and webhook event subscription | Persisted and visible via the CLI |
| TD-E-15 | Cluster page with a stopped node (cluster suite) | Offline node/disks highlighted, healing visible on restart |
| TD-E-16 | Live trace while the CLI runs operations | Lines appear in real time |
| TD-E-17 | Session expired during use | Redirect to login without losing the route |
| TD-E-18 | Dark theme, English language, 375 px mobile viewport | Layout without overflow |
| TD-E-19 | Accessibility: `@axe-core/playwright` on all the pages | No serious/critical violation |
| TD-E-20 | Visual regression (screenshots) on the main pages | Diff < threshold |

### Console security
| ID | Case | Expected |
|----|------|----------|
| TD-S-01 | Mutating request without CSRF token | 403 |
| TD-S-02 | Session cookie: `HttpOnly`, `Secure` (with TLS), `SameSite=Strict`; the CSRF token is in no cookie; `Strict-Transport-Security` over TLS | Verified |
| TD-S-03 | Object key with `<script>` shown in the browser | No execution (escaping + CSP) |
| TD-S-04 | Preview of an uploaded HTML/SVG file | Served with `Content-Disposition: attachment` or sandboxed, never executed in the console origin |
| TD-S-05 | `Content-Security-Policy`, `X-Frame-Options: DENY`, `Referrer-Policy` headers | Present on index.html |
| TD-S-06 | Non-admin user calls `/api/v1/users` directly | 403 (authorization always server-side) |
| TD-S-07 | Path traversal on static assets (`/../../etc/passwd`, encoded) | 404 |
| TD-S-08 | HTML page served by the S3 port of the same host (presigned link) that sends `no-cors` form POSTs to the Admin API with the session cookie and a leaked CSRF token (`/service/stop`, `/auth/logout`, `download-zip`) | All rejected (403 `invalid_csrf_token`: `Sec-Fetch-Site` not `same-origin`); no cookie readable with the token; server and session intact |
| TD-S-09 | Admin API answers (session with the CSRF token, listings, errors) | `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: SAMEORIGIN`; no HSTS over plain HTTP |
| TD-S-10 | Console login, then the audit log searched by its request id | The record names the user who signed in |

### Dashboard performance
- Initial bundle ≤ 300 KiB gzip (Monaco and charts lazy-loaded) — `size-limit` check in CI.
- Lighthouse performance ≥ 90 on Overview and Object browser.
- Object browser with 100k objects in the prefix: first render < 1 s, smooth scrolling.

## 6.6 Server performance tests

| ID | Test | Indicative target (4 NVMe nodes, 25 GbE) |
|----|------|------------------------------------------|
| TP-01 | `warp put` 64 MiB | ≥ 80% of the network bandwidth |
| TP-02 | `warp get` 64 MiB | ≥ 90% of the network bandwidth |
| TP-03 | `warp mixed` 1 KiB–1 MiB | ≥ 10k ops/s per node, p99 < 50 ms |
| TP-04 | Listing 1M objects in a prefix | ≥ 10k keys/s |
| TP-05 | Go benchmarks (`erasure`, `sigv4`, `xl.meta`) compared against the baseline (`benchstat`) | No regression > 10% |
| TP-06 | Steady-state memory under load | Stable, no leak in 24 h |

## 6.7 Mode, release and presigned tests

The specific tests are defined in the dedicated documents and are part of the pipeline:
- **Modes and release** (single node vs cluster, binary, Docker, Compose, migration): [08 §8.6](08-deployment-modes.md#86-mode-specific-and-release-tests) — TM-01…TM-12.
- **Presigned URLs, POST policy, access policies, share links** on server, CLI and dashboard: [09 §9.6](09-presigned-policy-sharing.md#96-tests) — TPS-*, TCL-P-*, TD-P-*.

CI matrix:

| Suite | SNSD | SNMD (4 disks) | Cluster (4×2, Docker) |
|-------|------|----------------|------------------------|
| S3/IAM/presigned integration | every PR | every PR | merge to `main` + nightly |
| CLI testscript | every PR | merge | nightly |
| Playwright dashboard | every PR (smoke) / merge (full) | — | nightly |
| s3-tests / mint | nightly | nightly | nightly |
| Docker image and binaries smoke | every PR | — | every PR (compose up + basic tests) |
| Multiple proxies in front of the deployment (`test/proxy`, TCD-01..03, TCC-C-05; `proxy` job) | every PR | every PR | every PR (in-process Go proxy and real nginx/HAProxy of `compose.multi-proxy.yml`) |

Tests of **access rules, domains and distributions** (server, CLI, dashboard, cluster with multiple proxies):
[10 §10.6](10-domains-access-distributions.md#106-tests) — TAR-*, TDS-*, TCL-A-*, TCL-D-*, TD-A-*, TD-D-*, TCD-*.

Tests of the **per-domain cache** (server, cluster, CLI, dashboard): [11 §11.12](11-domain-cache.md#1112-tests) — TCC-*, TCC-C-*, TCL-C-*, TD-C-*.

## 6.8 Traceability matrix

Every feature of the [catalog](02-features.md) must have at least one associated test. The matrix is
maintained in [`test/TRACEABILITY.md`](../test/TRACEABILITY.md) (generated from tags in the tests, e.g. `// covers: S-OBJ-06`) and checked in CI:
a P0/P1 feature without tests fails the release pipeline.

- **Tags**: a `covers: <code>[, <code>...]` comment (`//` in Go and TypeScript tests, `#` in `testscript`
  `.txtar` scripts, shell and Python) in the doc comment or in the body of the test; a value ending with a
  comma continues on the next comment line; the groups of the catalog can be written as a range
  (`D-ACC-01..04`). The text after the codes is free (e.g. `covers: S-OBJ-13 (--enc-c)`); the test case IDs
  (`TS-I-05`, `TCL-E-03`, `TD-E-11`, ...) stay in the titles and comments but do not count for the matrix.
- **Sources**: `*_test.go`, `test/**/*.txtar`, `web/src/**/*.test.ts(x)`, `web/e2e/**/*.spec.ts`, `.sh`/`.py` scripts
  (e.g. `packaging/smoke.sh`); the associated test is the Go function, the title of the Vitest or Playwright test/suite that
  follows the tag, or the whole script.
- **Tool**: `make traceability` (`go run ./cmd/tools/traceability`, logic in `internal/tools/traceability`,
  outside the release binaries) rewrites the matrix: summary per priority, P0/P1 features per area with their tests
  (the first 8 and the count), P2 features listed as "not in v1.0". The grouped codes of the catalog (`D-ACC-01..04`,
  `D-CCH-01..08`, `D-DST-01..08`) are expanded with the descriptions of [10](10-domains-access-distributions.md) and
  [11](11-domain-cache.md).
- **Check**: `make traceability-check` (`lint-go` CI job and release workflow) and the Go test
  `TestRepositoryTraceability` fail if a P0/P1 feature has no tests, if a tag names a code outside the
  catalog or if `test/TRACEABILITY.md` is stale.
