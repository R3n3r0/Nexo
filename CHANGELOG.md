# Changelog

All notable changes to Nexo. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
versions follow [Semantic Versioning](https://semver.org/). The codes in parentheses (`S-OBJ-06`, `D-MON-01`,
`C-ADM-08`) are those of the [feature catalog](docs/02-features.md); the tests that cover them are in the
[traceability matrix](test/TRACEABILITY.md).

## [1.0.0] - unreleased

First stable release: milestones M0–M9 of the [roadmap](docs/05-roadmap.md). Earlier releases were never
published: the baseline of the upgrade tests is the M7 commit (`f70cc7a`, built as `v0.7.0`).

### Deployment modes and storage
- A single binary for three modes detected from the arguments of `nexo server` (S-STO-00): single node and single
  drive without erasure coding (SNSD, S-STO-01), single node with multiple drives (SNMD, S-STO-02) and multi-node
  cluster (S-STO-06), with `format.json` and ellipses in the endpoints (`/mnt/disk{1...4}`,
  `http://node{1...4}/mnt/d{1...4}`).
- Reed-Solomon erasure coding with sets of 2 to 16 drives, parity per storage class `STANDARD` and
  `REDUCED_REDUNDANCY` (S-STO-03), bitrot protection with HighwayHash verified on read (S-STO-04).
- Healing: on read, persistent MRF queue, background scanner with sampled heal, drive replacement resumed after a
  restart, `admin heal` (S-STO-05, C-ADM-07, D-CLU-02).
- Cluster: internode RPC over HTTP/2 with msgpack and call-bound JWTs, quorum-based distributed locks (dsync,
  S-STO-07), coordinated bootstrap waiting for quorum, leader for singleton jobs (scanner, ILM, ACME),
  IAM/bucket/config invalidations across nodes, expansion with new pools and weighted placement (S-STO-08).
- Snapshot reads: a GET releases the lock after opening the data, so PUT and DELETE of the same key do not wait
  for the transfer.
- Export/import of the deployment state (IAM, configuration, bucket metadata) for backup and migration across
  modes (S-OPS-15, C-ADM-13).

### S3 API
- Buckets, objects, ranges and conditional requests, V1/V2 listing, CopyObject with `REPLACE` and
  copy-source-range, DeleteObjects, complete multipart with UploadPartCopy, user metadata (S-BKT-01..03,
  S-OBJ-01..07).
- SigV4 signatures in headers, query, `aws-chunked` streaming with trailers and `UNSIGNED-PAYLOAD`; presigned URLs
  with a maximum expiry of 7 days, POST policy, managed and revocable share links (S-IAM-02, S-OBJ-08, 08b, 08c).
- Versioning with delete markers and the `null` version, tagging, CORS, lifecycle (expiration, noncurrent,
  incomplete multipart uploads), Object Lock governance/compliance with legal hold, quotas, notifications (webhook,
  Kafka, NATS, AMQP, Redis, PostgreSQL) with a persistent queue, SSE-S3/SSE-KMS/SSE-C encryption (DARE) with a
  built-in KMS or KES and bucket default encryption, CRC32/CRC32C/CRC64NVME/SHA1/SHA256 checksums,
  GetObjectAttributes (S-BKT-04..11, S-OBJ-09..13, S-OBJ-15).
- Measured compatibility: Ceph `s3-tests` 451/451 applicable tests and MinIO `mint` 638 tests without unexpected
  failures, in SNSD, SNMD and cluster; intentional differences from Amazon S3 in [02 §2.4](docs/02-features.md).

### IAM, access and security
- Root user from the environment or files (`*_FILE`), users, groups, AWS-format IAM policies with conditions and
  variables, built-in policies (`readonly`, `readwrite`, `writeonly`, `consoleAdmin`, `diagnostics`), simulator and
  effective permissions, service accounts with inline policy and expiry, STS AssumeRole (S-IAM-01..07).
- Per-bucket access rules with prefix, glob and RE2 regex patterns, conditions (IP, referer, host, HTTPS, time
  window, headers, groups), public presets, block public access and tester (S-ACC-01..04).
- TLS from `--certs-dir` with SNI, hot reload (fsnotify and SIGHUP) without dropping connections, TLS ≥ 1.2
  (`--tls-min-version`), optional mTLS between nodes (S-OPS-02).
- Console sessions in `HttpOnly`/`SameSite=Strict` cookies, CSRF, origin checks even across ports of the same
  host, login throttling; IAM secrets encrypted at rest.
- STRIDE threat model ([docs/security/threat-model.md](docs/security/threat-model.md)), `make security`
  (govulncheck, gosec, `npm audit`), 24 fuzzing targets (`make fuzz`).

### Domains, distributions and cache
- CloudFront-style distributions: multiple domains (wildcards included) per bucket or prefix, `website` mode
  (index, error page, SPA fallback, redirects) and `s3` mode, per-path behaviors, signed URLs and cookies with key
  groups, trusted proxies, PROXY protocol, origin secret, IP restrictions, per-domain certificates uploaded or from
  ACME HTTP-01/TLS-ALPN-01/DNS-01, per-distribution metrics and access logs (S-DST-01..06, S-DST-08).
- Built-in memory and disk cache with policies per distribution, domain and path, quotas, automatic invalidation
  on write, purge by path/glob/regex/tag/domain, webhooks to external CDNs, cluster-shared cache,
  stale-while-revalidate/if-error, request collapsing and range caching (S-DST-07, 07b, 07c).

### Video streaming (HLS)
- MP4 videos (H.264 or H.265, optional AAC) play as HLS streams packaged on the fly, without transcoding: the
  `moov` index is read with ranged reads and cached by ETag, the segments are cut at the keyframes and built as
  fMP4 with one ranged read each. Limits on the index, the segments and the sample tables guard against forged
  files; a fuzz target covers the parser.
- The console previews the videos as HLS (hls.js loaded on demand, progressive fallback) under
  `/api/v1/buckets/{bucket}/objects/hls/`, with the authorization of a download.
- The S3 listener serves the streams under `/nexo/media/v1/{bucket}/` to whoever may read the object: the URIs
  of a presigned master playlist are presigned by its signer until the same expiry. Stream share links (`stream`
  in POST /shares, **Stream as a video** in the console, `nexoctl share download --stream`) give a public player
  page (`/nexo/media/player/`) and the playlist for any HLS player, and are revoked like any share. The video
  streams count against the request limits of the S3 API.
- Media worker (`nexo-media-worker`, a separate binary and image): the media rules of a bucket (Admin API
  `/buckets/{bucket}/media`, `nexoctl media`, the **Media** tab of the console) select videos by prefix and suffix;
  their jobs reach the worker through the persistent queue of the new `notify_media_worker` target, and the worker
  derives H.264/AAC renditions (presets 240p to 2160p, never taller than the source), a JPEG poster and a timeline
  thumbnail sprite with its WebVTT map. It uses OpenH264 (the Cisco build, downloaded and checksum-verified at the
  first start) instead of ffmpeg, keeps its jobs in a durable queue on disk, and reads and writes through the S3
  API only, under the reserved prefix `.nexo-media/`, with a least-privilege policy. The master playlist lists the
  renditions once `status.json` is done for the current ETag; the poster, the sprite, the thumbnails and the state
  are served next to the playlists.
- Video buckets in one step: the **HLS video** option of bucket creation (console switch, `nexoctl mb --with-hls`,
  `"hls": true` in `POST /buckets`; `nexoctl media rule add --hls` for an existing bucket) sets the media rule
  `hls`: every video gets every rendition from 240p to 2160p (up to its own height), a poster and thumbnails.
- Images in every format and width: an image rule of a bucket (the **Images in every format** option of bucket
  creation, `nexoctl mb --with-images`, `"images": true` in `POST /buckets`, `nexoctl media rule add --images`)
  makes the media worker turn every uploaded image (JPEG, PNG, GIF, WebP, AVIF, HEIC, TIFF, BMP) into AVIF, WebP,
  JPEG and PNG variants at its own size and at 160 to 2560 pixels wide, upright (EXIF orientation) and without
  metadata. `/nexo/media/v1/{bucket}/image/{key}?w=` and the console route serve the narrowest variant wide
  enough in the most compact format the client accepts (`Accept`, `Vary: Accept`; PNG rather than a flattened
  JPEG for a transparent image), with conditional requests and ranges; `nexoctl share download --image` makes a
  revocable link. Pure Go decoders and encoders (no cgo), encoded in parallel within a memory budget
  (`--image-memory`, `--image-max-pixels`, `--image-max-bytes`); the scaler of the videos moved to
  `internal/mediaworker/resample` and is shared.
- Images in every format on the same URL: a behavior of a distribution with `images: {negotiate: true}`
  (`nexoctl dist behavior add --negotiate-images`, **Images in every format** in the behavior editor of the
  console) serves the variants at the URL of the image itself: AVIF, WebP or JPEG as the `Accept` header allows
  (`Vary: Accept`), `?w=640` for a width, `?format=webp` for a format, `?format=original` (and every object without
  current variants) for the original. The cache keys the variants by format and width, never by `Accept`; an
  invalidation of the path of the image drops its variants and the `status.json` of the worker purges the URL at
  the CDNs in front; the query of a signed URL is signed. `GetObject` on the S3 port answers a variant only with
  `x-nexo-image-format`/`x-nexo-image-width` (signed with the query; plain `GetObject` is unchanged), and the
  Admin API image route answers `HEAD`.
- Video streaming on the domains of the distributions: a behavior with `video: {hls: true}` (`nexoctl dist
  behavior add --stream-video`, **Video streaming (HLS)** in the behavior editor of the console) serves
  `<video>/master.m3u8` and every file its playlists name below the URL of the video, renditions of the media
  worker included. The files pinned to a revision are cacheable for a day (`immutable`, public on a public
  behavior); a path that is not a streamable video is served as its object. On a signed behavior the playlists
  carry the parameters of a custom policy covering `…/video.mp4/*` to their URIs.
- The policy action `nexo:GetMediaStream` streams a video without allowing the download of the original: the
  console routes, the `/nexo/media/v1/` routes of the S3 listener and the video behaviors serve whoever is allowed
  it or `s3:GetObject`. Bucket policies may grant it (`Principal: "*"`), the access rules have the class `stream`
  (and `read` includes it), and the console policy editor and simulator know it.
- Subtitles: the master playlists list the subtitle tracks of a video as an `EXT-X-MEDIA` group (one WebVTT per
  language with its `X-TIMESTAMP-MAP`) on the console, the S3 listener and the video behaviors. A media rule with
  `subtitles` makes the worker transcribe the speech (an OpenAI-compatible endpoint: OpenAI, whisper.cpp server,
  faster-whisper-server; the audio copied into M4A parts without decoding) and translate it (LibreTranslate or an
  OpenAI-compatible chat model). Manual tracks (WebVTT or SubRip upload, cue editor in the console, Admin API
  `/buckets/{bucket}/objects/subtitles`, `nexoctl media subtitles`) always apply and are never overwritten by the
  worker: the automatic files live apart and the manifest is updated conditionally. Automatic tracks follow the
  content they were made from; a failed transcription does not fail the job (`subtitlesError` in the status).
- Benchmarks of every step of the video pipeline (`make bench-media`, [docs/bench/media.md](docs/bench/media.md)):
  packager requests, OpenH264 decoding and encoding per size, scaling, renditions, poster and thumbnails. With
  them the scaler (weights computed once, buffers reused, vertical pass first, parallel bands) and the decoding
  (overlapped with the encoders) bring three renditions of a 1080p video from 0.92× to 2.1× real time, and the
  video split at its IDR pictures into spans decoded and encoded on every core to 3.2× (4 vCPU Xeon virtual
  machine at 2.1 GHz; the output is the same byte for byte on any number of cores; the thumbnails decode their
  keyframes in parallel too, 28× → 74×); `make bench` and `make bench-media` save the description of the machine with their
  results (`test/bench/machine.sh`); a C call
  of the OpenH264 binding that could write to a moved goroutine stack was fixed.

### Observability (M8)
- Prometheus metrics on `/nexo/metrics/v1/{cluster,node,bucket}`: API with latency histograms, drives, network,
  locks, KMS, events, healing, scanner, ILM, cache and distributions; Grafana dashboards, alerts and Prometheus
  configurations in `deploy/docker/observability` (S-OPS-04).
- Time series for the console (24 h ring buffer per node, `/api/v1/metrics/query`).
- Live trace and live logs aggregated from all nodes over SSE, with filters and secret redaction; structured JSON
  or text logs with level and format changeable at runtime (S-OPS-05, S-OPS-07).
- Audit of every authenticated request with search and webhook/Kafka targets (S-OPS-06, D-MON-03).
- OpenTelemetry OTLP (`http/protobuf`, `grpc`) with one span per request and per internode call and `traceparent`
  propagation.
- Diagnostics: drive, object layer and network speed tests, pprof profiles protected by `admin:Profiling`
  (S-OPS-11, brought forward from M10).

### Admin API, console and CLI
- REST Admin API (`/api/v1`, contract in [api/openapi.yaml](api/openapi.yaml)) with per-action `admin:*`
  authorization, `problem+json` errors and the Go client `pkg/nexoadmin`.
- React console embedded in the binary: overview, buckets and settings, object browser with multipart upload, zip
  download, previews, versions, retention, users, groups, policies (Monaco editor and builder), access keys,
  shares, access rules, distributions, cache, cluster and healing, metrics, live trace and logs, audit,
  configuration; light/dark theme, English/Italian, no serious/critical axe violations (D-*).
- `nexoctl`: aliases, data commands (`ls`, `cp`, `mv`, `rm`, `mirror --watch`, `find`, `du`, `diff`, `tree`,
  `share`, `presign`, `anonymous`), bucket settings, `access`, `dist`, `cache`, `admin` (users, groups, policies,
  access keys, config, heal, info, trace, logs, audit, metrics, prometheus generate, speedtest, profile, cluster
  export/import), `--json` output, shell completion, `--debug` with redacted secrets (C-*).

### Packaging and deployment
- Static binaries for Linux, macOS and Windows (amd64/arm64), archives, deb/rpm packages with a systemd unit and a
  system user, Homebrew cask, SPDX SBOMs (S-OPS-12, C-UX-00).
- Multi-arch distroless Docker images `nexo` and `nexoctl` signed with cosign, `nexo healthcheck` with TLS,
  `*_FILE` secrets, Compose files for single node, SNMD, cluster (also with TLS and multiple proxies), Swarm stack,
  Helm chart `standalone`/`distributed` (S-OPS-13).
- Container images published only on Docker Hub: `docker.io/paolor/nexo` and `docker.io/paolor/nexoctl`
  (namespace from the repository variable `DOCKERHUB_NAMESPACE`, default `paolor`; secrets
  `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN` required), multi-arch, signed with cosign keyless, with SBOM/provenance
  attestations; the release also updates the Docker Hub description from `deploy/docker/DOCKERHUB.md`. GitHub
  Container Registry is not used. The Helm chart `nexo-X.Y.Z.tgz` is attached to the GitHub release with its
  cosign bundle (`helm install nexo https://github.com/R3n3r0/Nexo/releases/download/vX.Y.Z/nexo-X.Y.Z.tgz`).
- `nexo server --dry-run` and public URL (`NEXO_SERVER_URL`) for links behind a proxy (S-OPS-14).

### Documentation
- User documentation in [docs/user](docs/user/README.md) (quickstart, installation, modes, configuration,
  operations guide, security, distributions, references) with a MkDocs site and a CLI reference generated by
  cobra.
- `warp` benchmarks and profiling in [docs/bench](docs/bench/README.md).
- Traceability matrix [test/TRACEABILITY.md](test/TRACEABILITY.md): all 147 P0/P1 features have at least one
  test; otherwise `make traceability-check` blocks the release.

### Changed
- Go toolchain 1.26.9 (`go.mod`, Dockerfile, CI) and golangci-lint v2.14.0; upgraded `x/net` v0.60.0, `x/text`
  v0.42.0, pgx v5.9.2, OpenTelemetry v1.45.0, grpc v1.83.2, `klauspost/compress` v1.18.7, chi v5.3.0: `govulncheck`
  reports no reachable vulnerabilities.
- Go toolchain upgraded to 1.27.2 (`go.mod`, `golang:1.27.2-alpine` builder images in the Dockerfiles; CI reads
  `go.mod`); golangci-lint v2.14.0 (built with Go 1.27) unchanged, no module dependency changes required. mockery
  v2.53.4 is now pinned in a separate tools module (`tools/go.mod`, run with `go tool -modfile`) built with
  `x/tools` v0.51.0, which reads the Go 1.27 export data; the regenerated mocks only differ in import grouping.
  govulncheck v1.8.0 (was v1.1.4, whose SSA builder panics on Go 1.27 code) in `make security` and CI.
  With Go 1.27 `encoding/json` is backed by the v2 implementation: the line and column of a policy JSON syntax
  error are now taken from the offending byte (`SyntaxError.Offset` counts it), and the race detector caught a
  `sync.WaitGroup` Add/Wait race in the RPC connection close (fixed by ordering both under the connection mutex).
- The internode RPC client uses the HTTP/2 of `net/http` instead of `x/net/http2` (deprecated).
- SIGHUP reloads only the TLS certificates; `POST /api/v1/service/restart` restarts the process in place.
- New policy actions `admin:Profiling` and `admin:Speedtest`, included in the built-in `diagnostics` policy.
- Catalog priorities: S-OPS-11, D-MON-03 and C-ADM-12, delivered early, move from P2 to P1.
- The project files (code comments, logs, deployment files, design documents, changelog) are in English; the
  console defaults to English, with Italian available from the language switcher.

### Fixed (final review)
- **Distribution alias takeover**: the deployment hosts (node endpoints, `localhost`, `--server-url`,
  `--browser-redirect-url`, `--domain`) are reserved; the `/nexo/` prefix (health, metrics, RPC) never goes
  through Host routing; the internode RPC skips only the virtual hosts of `--domain`; an alias under the wildcard
  of another distribution requires permissions on it; the S3 listener serves distributions only with
  `--s3-distributions` (`auto`/`on`/`off`) and `--no-distributions` disables them for recovery (S-DST-01).
- **Redundant snapshot reads**: a GET that has released the lock keeps the shards of all online drives open
  (remote handles over RPC), so bitrot or drives or nodes lost during the transfer do not truncate it even if a
  concurrent PUT or DELETE has already removed the data, even if it lasts more than an hour (the reading node
  renews its handles); the part count is correct. Opening and closing a handle costs the same regardless of the
  number of handles open on the node.
- **Stalled HTTP/2 clients**: an HTTP/2 connection that accepts no bytes for `NEXO_SEND_TIMEOUT` is closed with
  all its streams on every listener; previously an anonymous client that advertised large windows and stopped
  draining the socket held handlers, buffered bodies and the read lock of objects of more than 8 parts
  indefinitely. On HTTP/2 the stall limit measures the progress of each stream in chunks sized on the streams of
  the connection: a browser loading many resources in parallel over a slow network is no longer interrupted.
- `/nexo/health/cluster` reports the real nodes, drives, write quorum and maintenance (S-OPS-03); `admin info`
  reports the objects and used space from the scanner.
- License: Nexo is released under the GNU AGPL v3.0 only (`AGPL-3.0-only`, `LICENSE`) with additional attribution
  terms under AGPL section 7(b) (`NOTICE`), and is also available under commercial licenses
  (`COMMERCIAL-LICENSE.md`); contributions require the sign-off described in `CONTRIBUTING.md`. `LICENSE` ships in
  archives, deb/rpm packages, OCI images and the Helm chart (the chart package is checked by `make helm-lint`);
  `NOTICE` and `COMMERCIAL-LICENSE.md` ship in archives and OCI images (next to `/LICENSE`) and `NOTICE` in the
  packages. The console
  shows the license, the "Powered by Nexo" attribution and a link to the source code (AGPL section 13), and
  `nexo --version` and `nexoctl --version` print the license.
- Audit, metrics and trace report the bucket and object resolved like the S3 API, from custom domains and from
  virtual hosts written with the trailing dot (`bucket.s3.example.com.`); the audit does not record anonymous
  health probes or successful anonymous metrics scrapes, but always records requests with credentials (signature,
  token: even a failed attempt on `/nexo/health/*`) and rejected scrapes.
- JSON access logs of the distributions: the start of the request is `event_time`, not a second `time` key
  (rejected by Elasticsearch).
- Bootstrap: the node that starts after the peers of a new deployment logs at INFO that it is waiting for its
  empty drives; the WARN `empty local drives in an existing deployment` remains for replaced drives of a set with
  data.
- Internode mTLS: accepted clients are signed by the CAs in `CAs/` and have a SAN among the endpoint hosts.
- A single redaction predicate (signatures, credentials, tokens, passwords, policies and keys) for logs, log
  stream, audit, access logs and trace.
- `POST /api/v1/profile` instead of GET; the live streams (trace, logs) check the origin of the console.
- Errors: distinct S3 codes for read and write quorum (`SlowDownRead`, `SlowDownWrite`), lock not acquired
  (`XNexoLockTimeout`) and drive or node offline (`ServiceUnavailable`); the Admin API answers with a curated
  message and logs the error chain with the request id; the console translates `lock_timeout`, `write_quorum` and
  `read_quorum`; `nexoctl` formats Admin API errors like S3 ones, does not retry quorum errors, points to
  `nexoctl admin info` and reports an unreachable server as `cannot reach <url>: <cause>`; `stat`, `cp`, `mv`, `rm`
  and `share --raw` on a nonexistent bucket answer `bucket not found` (not `object not found`), and the hint about
  a nonexistent alias appears only for arguments that name a location, not for input files (policies, YAML).
- `nexo server --dry-run` also checks `--web-address`, `--web-tls-address` and `--acme-ca-cert`;
  `nexo --version` prints the version as `nexo version`.
- `make run-single`/`run-snmd` use absolute paths; `make run-cluster` puts the console on the S3 port + 1;
  `make security` runs govulncheck and gosec with the module toolchain even if the installed `go` is older.
- Docker: `nexo.env.example` without a default password, Prometheus and Grafana of the observability stack bound
  to `127.0.0.1` and without default credentials; in `compose.cluster.yml` the nodes and the load balancer are on
  the default project network (with the fixed address of the load balancer), so the `compose.observability.yml`
  overlay reaches the targets of `prometheus.cluster.yml` (verified in CI).
- CI: workflows clean for actionlint with shellcheck, lint and vet also of the tests with the `compose`,
  `upgrade` and `nexotest` tags.
- Documentation: removed `NEXO_BROWSER` and `NEXO_CONSOLE_SESSION_DURATION`, never read (the console session
  lasts 12 hours); quickstart aligned with the real output.

### Security (console penetration test, T-9.3)
- Penetration test of the console and of the Admin API against a local build (SNMD, with and without TLS),
  reported in [docs/security/pentest-2026-10.md](docs/security/pentest-2026-10.md) with the accepted risks.
- **PT-01**: the answers of the Admin API (the session with its CSRF token, users, keys, configuration) carry
  `Cache-Control: no-store` (unless a route sets its own policy, such as the downloads), and every answer of the
  console listener `X-Content-Type-Options: nosniff` and `X-Frame-Options: SAMEORIGIN`
  (`middleware.NewSecurityHeaders`).
- **PT-02**: the console listener sends `Strict-Transport-Security: max-age=31536000` to the clients that come over
  TLS, directly or through a trusted proxy.
- **PT-03**: a node serves at most 256 live trace streams and 256 live log streams at once
  (`trace.WithMaxSubscribers`, `logs.WithMaxSubscribers`); beyond them `GET /api/v1/trace` and `GET /api/v1/logs`
  answer 429 `too_many_requests`. Each stream held a buffer of up to 10000 entries and was visited by every
  request: open streams could exhaust the memory of a node.
- **PT-04**: the audit record (and the live trace event) of a console login names the user who signed in.
- **PT-05**: disabling a user or an access key revokes its console sessions and its managed share links for good,
  as a secret rotation does: enabling it again with the same secret no longer brings back the sessions opened
  before (nor the links). IAM moves a revocation epoch per user and per key on disable, the sessions are bound to
  it (on every node, across restarts), and the Admin API deletes the links of the user, or those created with the
  key.
- Authenticated OWASP ZAP active scan of the Admin API driven by `api/openapi.yaml` against a local build
  (`make zap-scan`, `test/security/zap-api-scan.sh`), results in the pentest report §6.
- Contributor agreement: the `DCO` workflow checks that every commit of a pull request is signed off by its author
  (`.github/scripts/check-dco.sh`, `make dco`), and a pull request template reminds the sign-off.

### Performance (O_DIRECT and zero-copy, T-9.2)
- Zero-copy downloads: the GET of a single-drive (SNSD) version without SSE hands its part files to `sendfile(2)`
  over HTTP/1.1 without TLS, so the bytes go from the page cache to the socket without a copy in user space
  (ranges, multipart objects, pinned and lock-held reads alike). The stall bound of downloads still applies per
  256 KiB piece, a piece being resumed only after 32 KiB of progress, so slow and stalled clients are treated as
  before; HTTP/2, TLS, erasure-coded and encrypted bodies are copied as before. Server CPU per GiB downloaded
  −66% on 64 MiB objects and −47% on 4 MiB ones ([docs/bench §7.1](docs/bench/README.md#71-zero-copy-downloads-kept)).
- O_DIRECT for large data files was implemented and measured slower than page cache + fsync on the reference VM
  (−30% on 4 MiB synced writes, −52% on SNMD 4 MiB PUTs, cold reads right after a PUT): not included
  ([docs/bench §7.2](docs/bench/README.md#72-o_direct-for-large-data-files-rejected)). The internode RPC cannot use
  `splice` (HTTP/2 data frames, TLS).
- New benchmarks: `BenchmarkCreateFile`, `BenchmarkCreateFileParallel` and `BenchmarkReadFileStream` of the local
  drive (`internal/service/storage`, file system chosen with `NEXO_BENCH_DIR`) and `BenchmarkCopyBodyFile`
  (`httpx`, sendfile against a buffered copy).

### S3 Select (S-OBJ-16)
- `SelectObjectContent` over CSV and JSON objects (plain, GZIP, BZIP2) with the S3 event stream: projections,
  `WHERE`, `LIMIT`, aggregates, `LIKE`/`BETWEEN`/`IN`/`IS NULL|MISSING`, `CAST` and the common string functions,
  CSV or JSON output. Not supported: Parquet, `ScanRange`, date functions.

### Transparent compression (opt-in, S-OBJ-14)
- `compression` configuration subsystem (`enable`, `extensions`, `mime_types`, applied to the next writes): the
  selected unencrypted versions are stored as S2 streams, one per part, with the block index of each part in its
  metadata so that range reads decompress from the block holding their start. Sizes, ETags, checksums, listings,
  quota and usage stay those of the content; every block carries a CRC-32C checked on read. Never compressed:
  already compressed formats, encrypted versions, objects small enough to be inlined.

### Metadata log (opt-in, `NEXO_METADATA_LOG`)
- Each drive can keep the metadata of its objects in append-only segments with group commit and an index in memory
  (docs/proposals/small-object-log.md, [Metadata log](docs/user/operations/metadata-log.md)): concurrent writes share
  one flush, small objects need no directory. 4 KiB PUTs at concurrency 20 on the reference VM: about 1800 → 3800
  objects/s, p99 30 → 16 ms; 7x at the storage level with 32 writers.
- Records with CRC-32C, replay at start (a torn record at the end is cut off), compaction of the segments that are
  mostly garbage (deletes and drops kept while an older segment remains), volume removal recorded as a drop.
- Durability: a write is acknowledged after `fdatasync`; a damaged record loses only itself (the replay resumes at
  the next valid record); a compacted segment counts as present until its removal is flushed; the stale `xl.meta`
  files of an interrupted conversion are removed before the drive serves (`PRUNE`). Every directory created for an
  object is now flushed before the write is acknowledged, with or without the log.
- Enabling converts the `xl.meta` files of the drives at start; disabling converts the log back (a reversible switch,
  needed before a downgrade). A drive emptied or replaced while the server runs starts an empty log for the healing.
- `storage` configuration subsystem, applied at once: `metadata_log_segment_mib` (64) and
  `metadata_log_memory_percent` (25% of the RAM, warning beyond it).
- The storage behavior suite, the object layer suites and the integration suite run in both layouts
  (`NEXO_TEST_METADATA_LOG=1`).
- Monitoring and compaction on demand: the `metadataLog` of the drives in `GET /info`, the `nexo_metadata_log_*`
  Prometheus families (segments, live bytes, index memory, group commits, flush time, compactions, damaged areas),
  `POST /storage/metadata-log/compact` and `nexoctl admin metadata-log status|compact`.

### Performance (small objects, bulk deletes)
- The audit records no longer go to the server log by default (`NEXO_AUDIT_LOG`, `--audit-log`; the `debug`
  level logs them anyway): one line per request cost throughput, a lot on a Windows console. They stay in memory
  for the console and the CLI and go to the audit targets as before.
- The scanner pauses while the S3 API is busy (the requests in progress against the CPUs), so its walk no longer
  competes with a burst of writes.
- `DeleteObjects` deletes up to 16 keys at once, each under its own lock, and reports them in the order of the
  request: the clean-up of a `warp` run is about 2.4 times faster (51,708 objects in 16 s).
- Small PUTs skip two needless `unlink` calls after the commit; on Windows the directory sync (an extra handle
  open with no effect on NTFS) is skipped.
- The new `xl.meta` of a write is staged in the object directory instead of the shared tmp directory of the drive,
  which every write contended for (28% of the CPU of a 4 KiB PUT benchmark): p99 250 → 30 ms.
- 4 KiB PUTs at concurrency 20 on the reference VM (SNSD): about 1450 → 1700 objects/s.

### Rate limiting (S-OPS-10, S-DST-08)
- Request rate limits of each node with token buckets (`internal/service/ratelimit`), configured through the
  dynamic configuration (subsystem `ratelimit`, applied at the next request on every node, all off by default):
  S3 requests per client address (behind the trusted proxies, IPv6 by /64), per access key and per bucket, Admin
  API requests per client address and per user. Beyond a limit the S3 API answers `503 SlowDown` in S3 XML and the
  Admin API `429 too_many_requests` (problem+json), both with `Retry-After`; health probes, metrics scrapes and
  the internode RPC are exempt.
- `api.requests_max` and `api.requests_deadline` are applied: the S3 requests a node serves at once, a request
  beyond them waiting up to the deadline for a slot before `503 SlowDown` (`0`, the default, is no limit; the
  description of `api.requests_max` said "automatic").
- The per-distribution rate limit (`restrictions.rateLimit`, docs/10 TDS-13), validated and stored until now, is
  enforced on the website routes (`429`, plain text) and on the s3 distributions (`503 SlowDown`), per client
  address or per distribution, after the address and origin-secret admission; console **Security** tab with the
  scope, `nexoctl dist restrict --rate-limit-per ip|distribution` and `--no-rate-limit`.
- Limits are per node: a cluster behind a load balancer admits up to the number of nodes times each limit
  ([configuration](docs/user/configuration.md#rate-limits)). Refusals are counted in
  `nexo_ratelimit_throttled_total{api,kind}` and marked in the audit log (tag `throttled`) and the live trace.

### ACME DNS-01 for the wildcard domains (S-DST-06)
- The automatic certificates of the distributions can be obtained with the **DNS-01** challenge, the only one a
  wildcard alias (`*.preview.example.com`) can use: `tls.challenge` (`http-01`, `tls-alpn-01`, `dns-01`; default
  the deployment's `NEXO_ACME_CHALLENGE`) and `tls.dnsProvider` per distribution. The leader publishes
  `_acme-challenge.<domain>` through a **DNS provider**, waits until every authoritative nameserver of the zone
  serves it (`NEXO_ACME_DNS_NAMESERVERS` to check fixed nameservers instead), validates and removes it; the same
  `golang.org/x/crypto/acme` client as HTTP-01 and TLS-ALPN-01 (`internal/service/certs/dns01`).
- DNS providers: **Cloudflare** (API token), **AWS Route 53** (access key or the ambient credentials of the
  nodes, `aws-sdk-go-v2/service/route53` and `config`) and **RFC 2136** (TSIG-signed dynamic updates,
  `github.com/miekg/dns`, BSD-3-Clause). Admin API `/dns-providers` (create, list, get, replace, delete, test;
  `admin:ManageCertificates`), `nexoctl admin dns-provider add|ls|info|edit|rm|test`, `nexoctl dist cert set
  --auto --challenge --dns-provider`, console **Certificates** page (providers, test) and **TLS** tab (challenge
  and provider). Problem codes `dns_provider_not_found`, `invalid_dns_provider`, `dns_provider_exists`,
  `dns_provider_in_use`.
- The provider credentials are sealed (AES-256-GCM) in the system store, write-only in the API (`hasCredentials`;
  empty fields keep the stored values), redacted in the logs and never part of an error.
- **Behavior change**: saving a distribution with a wildcard alias and `tls.certificate: auto` now requires
  `tls.challenge: dns-01` with an existing `tls.dnsProvider` (before, the wildcard was silently left out of the
  certificate); the distributions already stored keep working unchanged until their next edit.

### Upgrading
- The on-disk formats are unchanged from `v0.7.0` (`format.json` v1, `xl.meta` 1.x): upgrade and downgrade
  verified in SNMD and with a rolling upgrade of a 4-node cluster under load (`make test-upgrade`,
  [08](docs/08-deployment-modes.md)).
- **Before restarting the first node** (behavior changes that break a mixed cluster or an existing
  configuration):
  - with `--tls-internode-verify` the client certificates of the nodes are verified only with the CAs in `CAs/`
    of the certificates directory (and with the default leaf): a CA present only in the chain of `public.crt`,
    accepted by `v0.7.0`, is no longer enough. Copy the CA that signs the node certificates into `CAs/` on **all**
    nodes before the rolling upgrade, otherwise every upgraded node rejects the internode calls of its peers (which
    see its drives offline) until quorum is lost. `nexo server --dry-run` warns when `CAs/` is empty with
    `--tls-internode-verify` and a leaf that is not self-signed;
  - the S3 listener serves distributions only with `--server-url`/`--domain` (`--s3-distributions=auto`, the
    default) or with `--s3-distributions=on`: whoever serves distributions on port 9000 without those options must
    set `NEXO_S3_DISTRIBUTIONS=on` (or `NEXO_SERVER_URL`/`NEXO_DOMAIN`) before upgrading, or move them to the web
    listeners (`--web-address`);
  - during the rolling upgrade, reads coordinated by an upgraded node towards `v0.7.0` nodes (which lack
    `storage/open-file`) stay under lock until the end, as in `v0.7.0`.

### Known limitations
- Rate limits are per node (no cluster-wide counter) and bound requests, not bandwidth or connections: keep a
  reverse proxy with connection limits in front of public deployments (`deploy/proxy`).
- Not included (M10): bucket and site replication, pool decommission and rebalance, OIDC and LDAP, tiering, S3
  Select, compression, ACLs other than `private`, coordinated cluster restart and orchestrated upgrade. The
  DNS-01 propagation check does not follow a CNAME delegation of `_acme-challenge` (acme-dns style).
- O_DIRECT is not used (measured slower on the reference VM, docs/bench §7.2); zero-copy covers single-drive
  downloads over HTTP/1.1 without TLS only.

### Verification

Final integration on 2026-10-15 on the working tree (Linux amd64, 4 vCPU, Go 1.26.9, Node.js 22):

| Check | Result | Duration |
|-------|--------|---------:|
| `go mod tidy`, `gofmt -l .` | no changes, no files to format | — |
| `go build ./...` (also `-tags cluster`, `compose`, `proxy`, `upgrade`, `nexotest`) | ok | 32 s |
| `go vet ./...` (also with the tags) | ok | 54 s |
| `golangci-lint run ./...` v2.14.0 (also with the tags) | 0 issues | 35 s |
| `go generate ./...` (mockery v2.53.4) | idempotent | 8 s |
| `govulncheck` v1.1.4 | 0 reachable vulnerabilities | 25 s |
| `gosec` v2.29.0, `npm audit --omit=dev` | 0 issues, 0 vulnerabilities | 75 s |
| `go test -race -count=1 ./...` (59 packages, integration in SNSD, SNMD and cluster) | ok | 13 min 46 s |
| `make test-cluster` (4 `bin/nexo` processes, TC-01..15) | ok | 4 min 17 s |
| `make test-proxy` (Go proxy, real nginx and HAProxy) | ok | 28 s |
| `make test-upgrade` (`v0.7.0` → working tree, SNMD and rolling upgrade) | ok | 1 min 4 s |
| `make traceability-check` | 75/75 P0 and 72/72 P1 with tests | 2 s |
| `npm run gen:api`, `lint`, `typecheck` | idempotent, ok, ok | 2 s, 52 s, 22 s |
| Vitest | 67 files, 754 tests | 1 min 26 s |
| `npm run build`, initial bundle | ok, 305.76 kB gzip (limit 307.2) | 47 s |
| `make build-embedded` | ok | 43 s |
| Playwright (`chromium`, `snmd`, `cluster-smoke`, `cluster`) | 118 tests | 3 min 8 s |
| `make docs` | 238 CLI reference pages, deterministic | 4 s |
