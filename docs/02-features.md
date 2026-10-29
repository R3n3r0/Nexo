# 2. Feature catalog

Priority: **P0** = essential MVP · **P1** = v1.0 · **P2** = after v1.0.
Every feature has a code (e.g. `S-OBJ-01`) referenced in the [roadmap](05-roadmap.md) and in the [test plan](06-test-plan.md).
The tests covering each code are in the traceability matrix [`test/TRACEABILITY.md`](../test/TRACEABILITY.md)
(`covers: <code>` tags in the tests, [06 §6.8](06-test-plan.md#68-traceability-matrix)): with v1.0 every P0/P1
feature has at least one test. P2 features delivered early in v1.0 are marked "P1 (brought forward, was P2)".

## 2.1 Server

### Buckets
| Code | Feature | Priority |
|------|---------|----------|
| S-BKT-01 | CreateBucket / DeleteBucket / HeadBucket / ListBuckets, DNS-compliant name validation | P0 |
| S-BKT-02 | GetBucketLocation, configurable region | P0 |
| S-BKT-03 | Bucket policy (Put/Get/Delete), anonymous access per bucket **and per prefix** (private/download/upload/public) | P0 |
| S-BKT-04 | Versioning (Enabled / Suspended) | P1 |
| S-BKT-05 | Bucket tagging | P1 |
| S-BKT-06 | Per-bucket CORS | P1 |
| S-BKT-07 | Lifecycle (expiration, noncurrent expiration, abort of incomplete multipart uploads, transition to a remote tier P2) | P1 |
| S-BKT-08 | Object Lock (governance/compliance, default retention, legal hold), enabled at bucket creation or later on a bucket with versioning enabled | P1 |
| S-BKT-09 | Per-bucket quota (hard) | P1 |
| S-BKT-10 | Bucket event notifications | P1 |
| S-BKT-11 | Bucket default encryption (SSE-S3 / SSE-KMS) | P1 |
| S-BKT-12 | Bucket replication to another cluster (active-active) | P2 |
| S-BKT-13 | Basic canned ACLs (private, public-read) for compatibility only; until then the "BucketOwnerEnforced" Object Ownership applies (see §2.4) | P2 |

### Objects
| Code | Feature | Priority |
|------|---------|----------|
| S-OBJ-01 | PutObject / GetObject / HeadObject / DeleteObject | P0 |
| S-OBJ-02 | Range GET, conditional requests (If-Match, If-None-Match, If-Modified-Since…) | P0 |
| S-OBJ-03 | ListObjects V1 / V2 (prefix, delimiter, pagination, start-after) | P0 |
| S-OBJ-04 | CopyObject (also with REPLACE metadata, copy-source-range) | P0 |
| S-OBJ-05 | DeleteObjects (batches of up to 1000) | P0 |
| S-OBJ-06 | Multipart upload (Create, UploadPart, UploadPartCopy, Complete, Abort, ListParts, ListMultipartUploads) | P0 |
| S-OBJ-07 | `x-amz-meta-*` user metadata, Content-Type/Encoding/Disposition, Cache-Control, Expires | P0 |
| S-OBJ-08 | Presigned URLs GET/PUT/HEAD/DELETE and multipart, `response-*` overrides, max expiry 7 days (see [09](09-presigned-policy-sharing.md)) | P0 |
| S-OBJ-08b | POST policy: upload from an HTML form with conditions (`content-length-range`, `starts-with`, redirect) | P0 |
| S-OBJ-08c | Managed, **revocable** share links (a dedicated service account per link) + list of shares | P0 |
| S-OBJ-09 | S3 checksums (CRC32, CRC32C, SHA1, SHA256, CRC64NVME), Content-MD5 | P1 |
| S-OBJ-10 | ListObjectVersions, operations by versionId, delete markers | P1 |
| S-OBJ-11 | Object tagging | P1 |
| S-OBJ-12 | Per-object retention / legal hold | P1 |
| S-OBJ-13 | SSE-S3, SSE-C, SSE-KMS | P1 |
| S-OBJ-14 | Transparent compression (S2) for configured content types | P2 |
| S-OBJ-15 | GetObjectAttributes (brought forward to M7) | P1 |
| S-OBJ-16 | SelectObjectContent (CSV/JSON/Parquet) | P2 |
| S-OBJ-17 | Protected video streaming: HLS AES-128 encryption, playback tokens, simultaneous streams and pace limits, viewer watermark, original download refused (see [proposal](proposals/video-protection.md)) | P2 |

### Access rules, domains and distributions

Details in [10](10-domains-access-distributions.md).

| Code | Feature | Priority |
|------|---------|----------|
| S-ACC-01 | Per-bucket access rules with prefix / glob / **RE2 regex** patterns, principals, actions, priority, allow/deny | P0 |
| S-ACC-02 | Conditions: IP/CIDR, referer, host, HTTPS, time window, headers, regex groups vs `${aws:username}` | P1 |
| S-ACC-03 | Public presets (public-read, public-prefix…) and cluster/bucket block public access | P0 |
| S-ACC-04 | Rule tester/simulator | P0 |
| S-DST-01 | Distributions: multiple domains (wildcards included) per bucket/prefix, Host routing updated hot across the cluster | P0 |
| S-DST-02 | `website` mode (index, error page, SPA fallback, redirects) and `s3` mode (S3 endpoint on a custom domain) | P0 |
| S-DST-03 | Per-path behaviors (prefix/glob/regex): methods, access, cache-control, headers, CORS | P0 |
| S-DST-04 | Distribution signed URLs and cookies with key groups and rotation | P1 |
| S-DST-05 | Trusted proxies, PROXY protocol, origin secret for proxies, IP restrictions | P0 |
| S-DST-06 | Per-domain TLS: upload and automatic ACME (HTTP-01/TLS-ALPN-01; DNS-01 for the wildcard domains with Cloudflare, Route 53 and RFC 2136 providers, done in October 2026) | P1 (DNS-01 brought forward, was P2) |
| S-DST-07 | Built-in cache (memory + disk) with per-distribution / **per-domain** / per-path policies, per-domain quotas (see [11](11-domain-cache.md)) | P1 |
| S-DST-07b | Automatic invalidation on write, purge by path/glob/regex/tag/domain, soft purge, webhooks to external CDNs/proxies | P1 |
| S-DST-07c | Cache shared across the cluster (consistent hashing), stale-while-revalidate/if-error, request collapsing, range caching, warm-up | P1 / P2 |
| S-DST-08 | Per-distribution rate limit; per-distribution metrics and access log | P1 (rate limit brought forward, was P2 / P1) |

### Authentication and IAM
| Code | Feature | Priority |
|------|---------|----------|
| S-IAM-01 | Root user from env (`NEXO_ROOT_USER`, `NEXO_ROOT_PASSWORD`) | P0 |
| S-IAM-02 | AWS SigV4 signature (header, query, chunked streaming, unsigned payload, trailer) | P0 |
| S-IAM-03 | Users (create, disable, delete, secret change) | P0 |
| S-IAM-04 | IAM policies in AWS format (conditions, variables, NotAction…), built-in policies (`readonly`, `readwrite`, `writeonly`, `consoleAdmin`, `diagnostics`) | P0 |
| S-IAM-04b | Policy simulator (allow/deny + deciding statement) and effective permissions per user | P1 |
| S-IAM-05 | Groups and policy attachment to users/groups | P1 |
| S-IAM-06 | Service accounts / multiple access keys per user, with expiry and a restrictive inline policy | P1 |
| S-IAM-07 | STS AssumeRole (temporary credentials) | P1 |
| S-IAM-08 | OpenID Connect (AssumeRoleWithWebIdentity, console SSO login) | P2 |
| S-IAM-09 | LDAP/Active Directory | P2 |

### Storage, durability, cluster
| Code | Feature | Priority |
|------|---------|----------|
| S-STO-00 | **Two modes with the same binary**: single node and cluster, detected from the arguments (see [08](08-deployment-modes.md)) | P0 |
| S-STO-01 | Single node, single disk (SNSD): no erasure, direct file system | P0 |
| S-STO-02 | Single node, multiple disks (SNMD): erasure coding with 2–16 disks per set | P0 |
| S-STO-03 | Configurable parity per storage class (`STANDARD`, `REDUCED_REDUNDANCY`) | P1 |
| S-STO-04 | Bitrot protection (HighwayHash) and verification on read | P0 |
| S-STO-05 | Healing: on-read, MRF, background scanner, disk replacement | P1 |
| S-STO-06 | Distributed multi-node cluster (see [03-cluster](03-cluster.md)) | P1 |
| S-STO-07 | Distributed quorum locks | P1 |
| S-STO-08 | Expansion through new pools | P1 |
| S-STO-09 | Pool decommission | P2 |
| S-STO-10 | Rebalancing between pools | P2 |
| S-STO-11 | Site replication (multiple clusters, synchronized IAM + buckets) | P2 |
| S-STO-12 | ILM tiering to remote S3 | P2 |

### Operations
| Code | Feature | Priority |
|------|---------|----------|
| S-OPS-01 | Config from flags/env/file + dynamic config via the Admin API, hot reload | P0 |
| S-OPS-02 | TLS (certificates from a directory, multiple SNI, hot reload) | P0 |
| S-OPS-03 | Health checks `/nexo/health/live`, `/ready`, `/cluster` | P0 |
| S-OPS-04 | Prometheus metrics (cluster, node, bucket, API, disks) | P1 |
| S-OPS-05 | Structured JSON (or text) logs on stderr, level and format changed hot; the HTTP/Kafka targets are those of the audit log (S-OPS-06), server logs are collected from the process stream | P1 |
| S-OPS-06 | Audit log (who, what, when, outcome) with search and webhook/Kafka targets | P1 |
| S-OPS-07 | Live request trace (stream) and live logs for the console | P1 |
| S-OPS-08 | Server info (version, uptime, disks, usage, network state) | P0 |
| S-OPS-09 | Coordinated cluster restart/stop, rolling binary upgrade | P2 |
| S-OPS-10 | Per-API rate limiting / throttling (per client address, access key/user, bucket; S3 concurrency `api.requests_max`; per node) | P1 (brought forward, was P2) |
| S-OPS-11 | Diagnostics: drive/net/obj speedtest, pprof profiling (protected) | P1 (brought forward, was P2) |
| S-OPS-12 | Release as a static multi-OS/arch binary (tar/zip, deb, rpm, Homebrew) | P0 |
| S-OPS-13 | Multi-arch `nexo` and `nexoctl` Docker images, built-in healthcheck, `*_FILE` secrets, single/SNMD/cluster Compose files | P0 |
| S-OPS-14 | `nexo server --dry-run` (configuration validation), public URL for links behind a proxy | P1 |
| S-OPS-15 | Full export/import of IAM, config and bucket metadata (migration between modes, backup) | P1 |
| S-OPS-16 | Kubernetes operator: `NexoCluster` resource (pools as StatefulSets, Services, disruption budget), rolling replacement of the pods gated on the cluster health, pool expansion, claims of the drives never deleted | P2 |

## 2.2 Dashboard (React console)

| Code | Page / feature | Detail | Priority |
|------|----------------|--------|----------|
| D-AUTH-01 | Login | Access/secret key, credential error, session with expiry, logout | P0 |
| D-AUTH-02 | SSO login | OIDC button | P2 |
| D-DASH-01 | Overview | Used/free capacity, number of buckets/objects, node/disk state, throughput, charts | P0 |
| D-BKT-01 | Bucket list | Search, sorting, size, number of objects, creation date, creation/deletion | P0 |
| D-BKT-02 | Bucket detail – Settings | Versioning, object lock, quota, tags, encryption, access policy (private/public/custom) | P1 |
| D-BKT-03 | Bucket detail – Lifecycle | Rule CRUD with a guided form + JSON view | P1 |
| D-BKT-04 | Bucket detail – Events | CRUD of subscriptions to configured targets | P1 |
| D-BKT-05 | Bucket detail – Replication | Replication rules, status | P2 |
| D-OBJ-01 | Object browser | Folder navigation (prefix/delimiter), breadcrumb, virtualized infinite pagination | P0 |
| D-OBJ-02 | Upload | Drag&drop of files and folders, multipart, progress, pause/resume/cancel, queue | P0 |
| D-OBJ-03 | Download | Single object, multiple as zip (server-side streaming) | P0 |
| D-OBJ-04 | Object actions | Delete (also recursive prefix), rename/move (copy+delete), create folder, share (presigned with expiry) | P0 |
| D-OBJ-05 | Preview | Images, PDF, text, JSON, video/audio | P1 |
| D-OBJ-06 | Object detail | Metadata, tags, versions (restore, version deletion), retention, legal hold | P1 |
| D-IAM-01 | Users | List, create, disable, delete, assign policies/groups | P0 |
| D-IAM-02 | Groups | CRUD, members, policies | P1 |
| D-IAM-03 | Policies | List, JSON editor (Monaco) with validation, visual policy builder | P0 |
| D-SHR-01 | Share | Presigned link dialog (download/upload), expiry, content-disposition, QR, copy | P0 |
| D-SHR-02 | Upload generator | PUT URL and POST policy with HTML/curl/JS snippets | P1 |
| D-SHR-03 | Shares | List of active links, single/multiple revocation | P0 |
| D-POL-01 | Bucket access | Private/public/custom, per-prefix anonymous rules, bucket policy editor | P0 |
| D-POL-03 | Policy simulator and user effective permissions | | P1 |
| D-ACC-01..04 | Access rules | glob/regex rule editor with drag&drop priority, pattern tester, public presets, block public access | P0 |
| D-CCH-01..08 | Cache | Global cache page, per-distribution cache tab with per-domain overrides, policy editor, invalidation with per-node status, URL inspector, warm-up, webhooks, per-domain charts | P1 |
| D-DST-01..08 | Distributions | List, domains+DNS wizard, behavior editor with URL tester, error page/redirects/headers, key groups and signed URLs, origin secret, certificates, invalidation and metrics | P0/P1 |
| D-IAM-04 | Access keys | Create a service account with inline policy and expiry, show the secret only once, credentials download | P1 |
| D-MON-01 | Metrics | Charts of requests, errors, latencies, bandwidth, disk usage; time range | P1 |
| D-MON-02 | Live logs and trace | Stream with filters (node, API, status), pause | P1 |
| D-MON-03 | Audit | Audit event search | P1 (brought forward, was P2) |
| D-CLU-01 | Cluster | Pools, nodes, disks (state, usage, healing), erasure set state | P1 |
| D-CLU-02 | Healing | Heal start, progress, repaired objects | P1 |
| D-CLU-03 | Pools | Expansion (instructions), decommission, rebalance | P2 |
| D-CFG-01 | Configuration | Region, notification targets, log targets, KMS, compression, scanner | P1 |
| D-CFG-02 | License/Info | Version, build, deployment ID | P0 |
| D-UX-01 | Light/dark theme, it/en i18n, responsive, WCAG 2.1 AA accessibility | | P1 |
| D-UX-02 | UI access control | Menus and actions visible according to the user's effective permissions | P0 |

## 2.3 CLI (`nexoctl`)

Syntax: `nexoctl <group> <command> [ALIAS/BUCKET/PREFIX] [flags]`. `--json` output on every command, stable exit codes.

| Code | Command | Description | Priority |
|------|---------|-------------|----------|
| C-CFG-01 | `alias set/list/remove` | Management of endpoints and credentials in `~/.nexoctl/config.json` (0600 permissions) | P0 |
| C-OBJ-01 | `ls [-r] [--versions]` | List buckets/objects | P0 |
| C-OBJ-02 | `mb` / `rb [--force]` | Create / remove buckets | P0 |
| C-OBJ-03 | `cp [-r]` | Copy local↔remote, remote↔remote, parallel multipart, progress bar | P0 |
| C-OBJ-04 | `mv`, `rm [-r] [--versions]` | Move, delete | P0 |
| C-OBJ-05 | `cat`, `pipe`, `head`, `stat` | Stream to stdout/from stdin, metadata | P0 |
| C-OBJ-06 | `mirror [--watch] [--remove] [--preserve]` | Directory/bucket synchronization; `--preserve` copies all the versions and delete markers between two buckets (migration, docs/08 §8.2) | P1 |
| C-OBJ-07 | `share download/upload [--post]`, `share list/revoke`, `presign --method` | Presigned URLs, POST policy, revocable share links | P0 |
| C-OBJ-07b | `anonymous set/get/list/set-json` | Per-bucket/prefix anonymous access and bucket policy | P0 |
| C-OBJ-08 | `find`, `du`, `diff`, `tree` | Utilities | P1 |
| C-OBJ-09 | `version enable/suspend/info`, `tag`, `retention`, `legalhold`, `ilm`, `event`, `quota`, `encrypt`, `anonymous` | Bucket/object settings | P1 |
| C-ADM-01 | `admin info` | Server/cluster status | P0 |
| C-ADM-02 | `admin user add/remove/enable/disable/list/info` | Users | P0 |
| C-ADM-03 | `admin group ...` | Groups | P1 |
| C-ADM-04 | `admin policy create/remove/list/info/attach/detach/entities/simulate` | Policies | P0 |
| C-ACC-01 | `access rule add/ls/info/rm/move`, `access test`, `access public`, `access block-public`, `access export/import` | Access rules | P0 |
| C-DST-01 | `dist create/ls/info/edit/rm`, `dist domain`, `dist behavior`, `dist keygroup`, `dist sign-url`, `dist cert`, `dist origin-secret`, `dist test`, `dist invalidate` | Distributions | P0 |
| C-CCH-01 | `cache policy ...`, `dist cache set [--domain] [--behavior]`, `cache purge/inspect/warm/stats/webhook` | Per-domain cache | P1 |
| C-ADM-13 | `admin cluster export/import` | Backup/migration of IAM, config, bucket metadata | P1 |
| C-ADM-05 | `admin accesskey create/list/remove/edit` | Service accounts | P1 |
| C-ADM-06 | `admin config get/set/reset/export/import` | Configuration | P1 |
| C-ADM-07 | `admin heal [-r] [--dry-run]` | Healing | P1 |
| C-ADM-08 | `admin trace`, `admin logs` | Live streams | P1 |
| C-ADM-09 | `admin service restart/stop` | Service control (restart and stop of the node receiving the request already in v1.0; of every node with `--cluster`, S-OPS-09) | P2 |
| C-ADM-10 | `admin decommission start/status/cancel`, `admin rebalance` | Pool management | P2 |
| C-ADM-11 | `admin replicate add/info/remove` | Site replication | P2 |
| C-ADM-12 | `admin speedtest`, `admin prometheus generate` | Diagnostics/integration | P1 (brought forward, was P2) |
| C-UX-00 | Distribution | Static binary for Linux/macOS/Windows and `nexoctl` Docker image | P0 |
| C-UX-01 | bash/zsh/fish/powershell completion, `--quiet`, `--no-color`, `--debug` (HTTP dump) | | P0 |

## 2.4 Known differences from Amazon S3

Design choices verified with the Ceph `s3-tests` and MinIO `mint` suites (`test/compat`, [06 §6.1](06-test-plan.md));
every test excluded for these reasons is listed with its rationale in `test/compat/excluded.txt` and
`test/compat/mint-excluded.txt`.

| Area | Nexo behavior | Reason |
|------|---------------|--------|
| Keys and prefixes | A key cannot also be the prefix of other keys (`a/b` and `a/b/c`): the second write is rejected with `XNexoParentIsObject` (400) | Objects are directories on the disks with `xl.meta` next to the data ([01 §1.5](01-architecture.md)), as in MinIO |
| ACL | Every bucket behaves like an S3 bucket with `BucketOwnerEnforced` Object Ownership (the AWS default since 2023): ACLs other than `private`, explicit grants and object ownership by the writer are rejected (`NotImplemented`); access is granted with bucket policies and anonymous access rules | S-BKT-13 (P2) |
| Account | A single account: IAM users have the rights of their policies, not those of "owner" of the buckets they create; an existing bucket always answers `BucketAlreadyOwnedByYou`, even in `us-east-1` | Nexo IAM model (S-IAM-*) |
| Signatures | SigV4 only (header, query, chunked, POST policy); no SigV2 (even `aws s3 presign` of AWS CLI v1 must be configured with `s3.signature_version = s3v4`). A signature scoped to another region is rejected (`AuthorizationHeaderMalformed`), as by a regional S3 endpoint: a deployment has a single region. In a presigned URL every `x-amz-*` header must be signed (`AccessDenied`, "headers present in the request which were not signed"), except `x-amz-content-sha256`, which, if malformed, makes the signature invalid (`SignatureDoesNotMatch`) | [01 §1.1](01-architecture.md), non-goals; an unsigned header would change the operation granted by the URL |
| Checksum algorithms | CRC32, CRC32C, CRC64NVME, SHA1, SHA256; the algorithms added by S3 in 2025 (MD5, SHA512, XXHASH64/3/128) are rejected (`InvalidRequest`) | S-OBJ-09 |
| Responses | As in S3: lower-case `x-amz-meta-*`, no `x-amz-version-id` for the `null` version in write responses, `If-Match` on a missing key → 404 `NoSuchKey`, `partNumber` beyond the parts → 416 `InvalidPartNumber`, different SSE-C key → 403, SSE-S3/KMS headers on GET/HEAD → 400, repeating an already successful `CompleteMultipartUpload` → same outcome, strictly increasing part list (`InvalidPartOrder`), `x-amz-delete-marker` absent when it would be `false`, `GetBucketPolicyStatus` without a policy → 404 `NoSuchBucketPolicy`, the version written before versioning was enabled is the bucket's `null` version (reported and addressable as `null` by reads, listings, lifecycle and console), `CompleteMultipartUpload` of a COMPOSITE upload without the checksum of every part → 400 `InvalidRequest` (with `BadDigest` first, if a given checksum does not match), `x-amz-mp-object-size` different from the sum of the parts → 400 `InvalidRequest`, a version of a missing key → 404 `NoSuchVersion` (not `NoSuchKey`), `NextPartNumberMarker` of `ListParts`/`GetObjectAttributes` always the last part of the page, `PutBucketTagging` → 200, empty `Content-MD5` → `InvalidDigest`, unknown canned ACL → 400 `InvalidArgument`, POST on an object without a subresource → 405 `MethodNotAllowed`, `ListObjectVersions` with versions and delete markers in listing order, copying an object onto itself changing only its encryption (e.g. decrypting) allowed, error messages with plain quotes (e.g. `Policy Condition failed: ["eq", ...]`), `Date` header with a `-0000` zone (botocore) accepted. `DeleteObject` with `If-Match` (ETag or `*`) deletes only if the target version has that ETag, otherwise 412 `PreconditionFailed` (also for a missing version or a delete marker); the directory bucket conditions (`x-amz-if-match-last-modified-time`, `x-amz-if-match-size`) → 501, never ignored. `AbortMultipartUpload` of an unknown upload → 404 `NoSuchUpload` (MinIO answers 204). The ACL `Grantee` repeats the type as a `<Type>` element (required by the MinIO SDKs, ignored by the AWS ones). `Expect` other than `100-continue` → 417 (Go HTTP server, RFC 9110) | Compatibility with the SDKs |
| Authorization | A request on an object is authorized (e.g. `s3:GetObject`) before looking up the key: a caller with only `s3:ListBucket` gets 403 even for missing keys; a caller with `s3:GetObject` gets 404 for a missing key even without `s3:ListBucket` | Policy evaluation independent of the state of the data |
| Non-ASCII metadata | `x-amz-meta-*` values are returned with the bytes received (UTF-8), without RFC 2047 encoding or transcoding to ISO-8859-1 | The Go and JavaScript SDKs read headers as UTF-8 |
| Concurrent reads and writes | A GET reads a snapshot: once the version is resolved, it opens the data files and releases the read lock immediately, so a PUT or DELETE of the same key does not wait for the transfer and the reader receives the resolved version in full even if in the meantime it was replaced and its data deleted (an open file survives removal, POSIX). With erasure coding the read keeps open the shard of **every** online drive: the local files, and those of the remote drives through a handle that their node keeps open without transferring data (`storage/open-file`, closed at the end of the read, on orderly shutdown of the reading node, at the first startup of its next process if it restarted after a crash, and in any case after 1 hour in which the reading node neither uses nor renews it: while the read is in progress it renews it every 15 minutes, even if it never reads that shard (`storage/touch-handles`), and a periodic sweep closes forgotten handles even without other reads); this way a shard added mid-read (bitrot, a drive or node going down, a slow shard) reads the same version and the read keeps all the redundancy of the parity. Reads of more than 8 parts (`object.WithPinnedReadParts`) and all reads on Windows stay under the lock, without opening anything beforehand; reads in which an online drive could not open its shard for a reason other than a missing shard (a node of a previous release during a rolling upgrade, the node's handle table full, network error) also stay under the lock until the end. A client that stops reading for more than 20 s (`NEXO_SEND_TIMEOUT`) still sees the response cut off, both on the S3 API and on the distribution sites (S3 and web listeners): the limit applies to every response of every listener, and an HTTP/2 connection that accepts no bytes for the same time is closed with all its streams | MVCC on the versions' data dirs; MinIO holds the lock for the whole transfer |
| CORS | As in S3, `Access-Control-Allow-Headers` reports the requested headers in lower case, and a non-preflight request that declares `Access-Control-Request-Method` is evaluated with that method. A bucket **without** a CORS configuration follows the deployment-wide policy `api.cors_allow_origin` (default `*`, [07 §7.6](07-security-operations.md)): the preflight answers 200 with the allowed origins instead of S3's 403 `CORSResponse: CORS is not enabled for this bucket`. A bucket with a CORS configuration behaves as in S3 (only its rules decide, 403 if none matches) | The console and web applications served from other domains work without configuring every bucket (like MinIO); can be restricted per deployment |
| Event notifications | The records (`Records[]`) have the S3 shape (`eventVersion` `2.1`, `eventName` without the `s3:` prefix, e.g. `ObjectCreated:Put`, URL-encoded key, `sequencer`), with `eventSource` `nexo:s3`; the webhook payload adds at the top `EventName` (with prefix, `s3:ObjectCreated:Put`) and `Key` (`bucket/key`) like MinIO. Overlapping configurations (same event type and intersecting prefix/suffix filters) are rejected with `400 InvalidArgument` as in S3. The `sequencer` is the commit time of the version (or of the removal), monotonic per key even across nodes with skewed clocks. Object Lock changes (retention, legal hold) do not generate events, as in S3: MinIO publishes `s3:ObjectCreated:PutRetention`/`PutLegalHold`, which a consumer of `s3:ObjectCreated:*` would read as new objects | Compatibility with consumers written for S3 and for MinIO |
