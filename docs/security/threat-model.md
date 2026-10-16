# Nexo threat model (STRIDE)

This document extends the summary in [docs/07 §7.1](../07-security-operations.md#71-threat-model-stride-summary)
with a STRIDE analysis per surface (M9 T-9.3). For each threat it gives the mitigation **in the code** (file and
mechanism) and the **residual risk**: what is left to the deployment or accepted. References are relative to the
repository root; the automated scans (govulncheck, gosec, npm audit) and fuzzing are described in §10 and §11, and the
consolidated residual risks in §12.

## 1. Scope, assets and trust boundaries

**Assets**: object data and metadata (encrypted too), credentials (root, IAM users, service accounts, STS sessions,
console tokens), keys (KMS master key, DEKs, clients' SSE-C keys, distribution signing keys, proxies' origin
secrets), configuration (IAM, policies, access rules, distributions, cache, event targets), service availability and
cluster integrity (drive format, quorum, distributed locks).

**Actors**: anonymous clients on the Internet (websites, signed links, public objects); authenticated S3 clients
(users, service accounts, STS sessions); administrators (console, Admin API, `nexoctl`); other cluster nodes;
external services called by Nexo (KES, event targets, purge webhooks, ACME); the machine operator (file system,
environment variables, signals).

**Trust boundaries**:

| Boundary | Inside | Outside |
|---------|--------|-------|
| S3 listener (`--address`, also distribution domains on opt-in) and the distributions' web listeners (`--web-address`, `--web-tls-address`) | handlers, services | any network client |
| Console/Admin API listener (`--console-address`) | `/api/v1` API, SPA | browsers and `nexoctl` |
| Internode RPC (`/nexo/rpc/v1/...` on the S3 listener) | nodes with the same root credentials and the same deployment id | any other peer |
| Egress (KES, events, webhooks, ACME) | Nexo | external services, third-party ones included |
| Drive file system, `--certs-dir`, `.nexo.sys` | `nexo` process (dedicated user) | other local users |

**Assumptions**: the system user running `nexo` and whoever controls its environment (variables, configuration
files, `--certs-dir`) are trusted; the drives are not writable by other users; the instance's root is a trusted
administrator (it bypasses policies by design). Reverse proxies in front of Nexo are trusted only if listed in
`--trusted-proxies`.

## 2. S3 API (`internal/transport/http/s3`, `internal/service/auth`, `internal/service/policy`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Forged or replayed signature | Full SigV4 with constant-time comparison and a cache of derived keys (`internal/service/auth/sigv4.go`, `keycache.go`); clock skew window (`RequestTimeTooSkewed`, `errors.go`); presigned URLs at most 7 days (`canonical.go` `maxPresignExpiry`); signed payload or explicit `UNSIGNED-PAYLOAD`, signed chunks verified one by one (`chunked.go`); POST policy with verified conditions (`postpolicy.go`); service scope (`scope.go`) | Replay of a signed request within the skew window (as in S3): use TLS |
| S | Stolen STS credentials | Expiring sessions, token bound to the key (`internal/service/iam/sts.go`); revocation on secret rotation | A stolen token is valid until it expires |
| T | Body altered in transit or on disk | Content-MD5 and `x-amz-checksum-*` checksums (`internal/transport/http/s3/body.go`, `internal/service/auth/checksum.go`, `internal/service/object/hash.go`); per-block HighwayHash bitrot hash (`internal/service/erasure/bitrot.go`); CRC of `xl.meta` (`internal/service/storage/xlmeta.go`); TLS | Without TLS, unsigned metadata (headers not included in `SignedHeaders`) can be modified by a MITM |
| T | Path traversal in keys (`../`, NUL, invalid UTF-8) | Key validation (`internal/domain/object.go`), paths resolved inside the drive with `O_NOFOLLOW` (`internal/service/storage/fsutil.go`), normalization shared with the access rules (`internal/service/match/normalize.go`) | — |
| T | Overwriting protected versions | Object Lock (retention, legal hold, governance bypass only with explicit permission, `internal/service/object/buckets.go`), versioning | Root can delete a bucket in governance mode with bypass, by design |
| R | Unattributable operations | Request id on every response (`middleware/requestid.go`), audit log with identity (`middleware/audit.go`, `internal/service/audit`), including requests rejected by authentication: only anonymous health probes and successful anonymous metrics scrapes are left out, never a request with credentials (signature, token) nor a rejected scrape; bucket and object resolved as by the S3 API before authentication (`middleware/target.go`, `httpx.VirtualHostBucket`) | Audit delivery to external targets is asynchronous: a crash can lose events not yet delivered |
| I | Unauthorized read | Deny-by-default policies evaluated server-side for every operation (`internal/service/policy`, `internal/transport/http/s3/authz.go`), glob/regex access rules and Block Public Access (`internal/service/accessrule`), generic error messages to anonymous users | A permissive bucket policy written by the administrator is honored |
| I | Data at rest readable by whoever holds the drives | SSE-S3/SSE-KMS/SSE-C, DARE stream with anti-truncation (`internal/service/crypto/stream.go`), sealed object keys (`internal/service/crypto/crypto.go`) | Unencrypted objects are in plain text on the drives; metadata (names, sizes) is not encrypted |
| D | Huge bodies/documents, nested XML, malformed chunks; clients that stop reading (socket not drained, HTTP/2 window at zero) to keep handlers, locks and buffers busy | Limits on XML/JSON documents (`maxXMLBody`, `maxCompleteBody` in `s3/body.go`, `s3/multipart.go`), body limit (`middleware/maxbody.go`), `ReadHeaderTimeout` and listener timeouts (`internal/transport/http/server/server.go`), maximum digits of the chunk size (`auth/chunked.go`), bucket quotas (`internal/service/quota`); stall limit on every write of every response of every listener (`server/stall.go`, `NEXO_SEND_TIMEOUT`, 20 s: the HTTP/1 response or the HTTP/2 stream is aborted) and on every HTTP/2 connection, closed with all its streams when it accepts no bytes for the same time (`HTTP2Config.WriteByteTimeout` in `server.go`: a stream reset cannot be sent if the client does not drain the socket); handlers, locks and the buffered body are thus released; net/http HTTP/2 windows (1 MiB per connection) and at most 128 streams per connection, larger windows (8 MiB per stream, 16 MiB per connection) only on the S3 listener of a cluster, which carries the RPC (`server.WithHTTP2Windows`) | Rate limits per client address, access key and bucket and the concurrency limit of the S3 API (`internal/service/ratelimit`, `middleware/ratelimit.go`: `ratelimit.*`, `api.requests_max`) are **off by default** and **per node** (N nodes behind a load balancer admit N times each limit); a client within the limits (even an anonymous one against public objects) can still saturate bandwidth: set the limits, or use a reverse proxy with limits (`deploy/proxy`). Toolchain vulnerabilities (§10.1) |
| E | Escalation from user to root | Policies grant no implicit `admin:*` actions; derived credentials (service accounts, STS sessions) are valid for the intersection between the inline policy and the parent's policies, root included (`internal/service/policy/evaluate.go`); a user manages only their own keys and a derived credential none, so it cannot mint a broader one (`internal/service/iam/ownership.go`) | — |

## 3. Admin API and console (`internal/transport/http/admin`, `internal/service/admin`, `internal/service/session`, `web/`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Session theft, forced login | Session cookie `HttpOnly`, `SameSite=Strict`, `Secure` if the connection (or the trusted proxy) is TLS (`admin/session.go`), `Strict-Transport-Security` on every answer of the console listener to a TLS client (`middleware/security.go`); session key HKDF(root, deployment id) (`internal/service/session/session.go`); session bound to the secret it was opened with and re-resolved in IAM on every request (a rotated secret, a disabled or removed user end it); disabling a user or an access key moves its revocation epoch in IAM, which ends its sessions (and those of the keys of the user) for good on every node, and revokes its managed share links (pentest PT-05); login throttling per user and per client (`session/throttle.go`), the same answer for an unknown user and a wrong secret; login only with an `application/json` body and not from another site (`Sec-Fetch-Site`); SigV4 for programmatic clients | Without TLS the cookie travels in plain text |
| S | CSRF | Mandatory CSRF header for requests with cookies, origin check also cross-port (`middleware/sessionauth.go`) | — |
| T | Manipulated requests (JSON) | Strict decoding (unknown fields rejected, size limits), validation in the services; end-to-end fuzzing (`test/fuzz/admin_test.go`) | — |
| R | Untracked administrative actions | Audit of every request with identity and action; the audit entry of a console login names the user who signed in (`admin/session.go` records it in the identity holder of the audit middleware) | See §2; a failed login is audited with its client address, not with the access key it tried |
| I | Internal details in errors | The `detail` of problem documents is a curated message per error class, or the cause the service phrases for the caller (invalid field, missing permission); the internal chain (packages, paths, drives) only goes to the request log with the request id (`httpx/errors.go`, `middleware/logging.go`); an unknown error is a 500 without detail | — |
| I | Secret leaks from the console | Secret key shown once; sensitive values masked in `GET /config`; restrictive CSP of the SPA (`internal/transport/http/console/spa.go`); downloads with a marker cookie without secrets (`admin/objects.go`); answers of the Admin API with `Cache-Control: no-store`, `X-Content-Type-Options: nosniff` and `X-Frame-Options: SAMEORIGIN` (`middleware/security.go`): no browser or shared cache keeps the session payload (CSRF token), the users or the keys; previews only for raster images, PDF, text, JSON, audio and video, sandboxed by CSP but PDF, everything else as an `attachment` (`internal/service/admin/objects.go` `previewType`) | A pprof profile (`POST /api/v1/profile`) contains stacks and memory data: it is reserved to `admin:Profiling` (root, `diagnostics` policy) |
| D | Profiling/speedtest as a load vector | One profile at a time (409), durations 1s–2m, speedtests with size/duration/concurrency limits and 1 GiB per target (`internal/service/admin/diagnostics.go`); dedicated actions `admin:Profiling`, `admin:Speedtest` | An authorized speedtest loads the drives and the network for its duration |
| D | Live streams (trace, logs) as a memory and CPU load vector | At most 256 live trace and 256 live log streams per node, 429 beyond them (`trace.WithMaxSubscribers`, `logs.WithMaxSubscribers`); the streams are opened only by the console itself or by signed clients (`admin/authn.go` `liveStream`); listings limited to 1000 entries per page | A holder of `admin:ServerTrace` or `admin:ConsoleLog` can occupy every stream of a node and keep other administrators from opening one |
| E | Routes without authorization | Every route declares its actions (`x-nexo-actions` in `api/openapi.yaml`), centralized guard (`admin/authz.go`), route ↔ spec consistency test (`admin/routes_test.go` `TestRouteTableMatchesSpec`); object operations with the same condition keys as the S3 API (`internal/service/policy/request.go`); ownership of access keys and shares checked by the services (a user manages only its own, derived credentials none, a delegated administrator never the keys of root: `internal/service/iam/ownership.go`, `internal/service/admin/accesskeys.go`) | Like `iam:CreateAccessKey` on AWS, `admin:CreateServiceAccount` and `admin:UpdateServiceAccount` on every user let their holder act as any user but root: grant them only to who administers the users |

## 4. Internode RPC (`internal/transport/rpc`, `internal/service/cluster`, `internal/app/foundation.go`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Foreign node pretending to be a peer | HS256 JWTs with key HKDF(root credentials, deployment id), audience `nexo-internode`, TTL 20s, bound to method, target and hash of the arguments (`rpc/auth.go`); optional mTLS (`--tls-internode-verify`: client certificate from the keystore, `internodeTLS` in `app/foundation.go`; verified only with the CAs in `CAs/` or the shared certificate, never with the intermediates of `public.crt`, and bound to a host of the nodes' endpoints: `certs/keystore.go`, `rpc.WithClientCertHosts`) | Whoever knows the root credentials can impersonate a node: protect them as cluster keys |
| T | Altered frames | TLS; CRC32 per frame (`rpc/frame.go`) | Without TLS the data between nodes is in plain text and modifiable (the CRC is not a MAC) |
| R | — | Logs of failed calls | — |
| I | Reading data between nodes | TLS between nodes (keystore of `--certs-dir`, CA in `CAs/`) | The *client* CA pool (peer verification) is hot reloaded, the *server* pool used by the RPC clients is read at startup: a new peer CA requires a restart (docs/07 §7.3) |
| D | Flood of failed authentications | Per-address failure limiter with a bounded table (`rpc/limiter.go`); message size limits | The limiter is per process, fail-open beyond the table |
| E | Execution of unexpected operations | Only registered routes (`rpc/routes.go`, `storage_routes.go`), typed msgpack arguments | — |

## 5. Distributions and websites (`internal/service/distribution`, `internal/transport/http/dist`, `internal/service/certs`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Someone else's domain hooked (alias takeover), Host spoofing | Unique, validated aliases (`distribution/validate.go`, `routing.go`). **Reserved** (exact and under a wildcard, rejected on save and left out of the routing table if saved earlier) are the domains of `--server-url`/`--browser-redirect-url`, `--domain` and its virtual hosts, the hosts of the nodes' endpoints, the hosts of `--address`/`--console-address`, `localhost` and the machine name (`app/distribution.go` `deploymentHosts`). The S3 listener never routes the reserved `/nexo/` paths by Host (health, metrics, internode RPC: `dist.WithPassthrough`, `s3` `hostBucket`, RPC without the distribution skip in `app/foundation.go`) and serves aliases only on opt-in (`--s3-distributions`: `auto` when `--server-url` or `--domain` declare the names of the S3 endpoint, `on`, `off`); the web listeners (`--web-address`, `--web-tls-address`) serve only distributions. An alias (exact or a more specific wildcard) under another distribution's wildcard also requires `admin:ManageDistributions` on that distribution's bucket (`admin/distributions.go` `authorizeCovered`). Forwarded headers only from trusted proxies (`middleware/forwarded.go`); origin secret for the proxies (`deploy/proxy`). Recovery without a working API: `nexo server --no-distributions` (no host routed, distributions editable) | DNS and proxies are the operator's responsibility; with `--s3-distributions on` (or with `auto` and other DNS names reaching the S3 port besides the declared ones) an alias can coincide with a name used by S3 clients: declare all the names in `--server-url`/`--domain` or serve the distributions on the web listeners |
| S | Forged signed URLs | Ed25519/RSA signatures per key group with expiry and constraints (`distribution/signing.go`, `keygroups.go`); fuzzing of `VerifySignedRequest` | A stolen private key of a key group signs valid URLs until rotation |
| T | Path traversal, double encoding, Unicode | Single key normalization (`match/normalize.go`: NFC, `.`/`..`), fuzzing `FuzzWebsitePath`, `FuzzNormalize` | — |
| I | Access to private objects via a website | Per-path behaviors (`access: public|signed|authenticated|deny`), Block Public Access, index/error pages without an existence oracle | — |
| D | Expensive requests on public domains | Per-domain cache (§6), request collapsing, per-distribution rate limit per client address or per distribution (`restrictions.rateLimit`, enforced by the host router after the address and origin-secret admission, so that refused clients spend no token: `dist/hosts.go`) | Per node and off unless configured (§2) |
| E | TLS certificate uploaded by a non-admin user | Upload reserved to `admin:ManageCertificates`; private key never returned | — |
| I/E | Credentials of the ACME DNS-01 providers (a DNS write access to the zones) | `/dns-providers` reserved to `admin:ManageCertificates`; credentials sealed with AES-256-GCM in the system store (provider name as additional data), write-only (only `hasCredentials` is returned, empty fields keep the stored values), redacted in `%v`/`slog` (`certs.DNSCredentials`), never part of an error (`internal/service/certs/dnsproviders.go`, `dns01/*.go`); Route 53 may use the ambient credentials of the nodes; the propagation check and the test of a provider only touch `_acme-challenge.<domain>` TXT records | Whoever holds the root credentials (the sealing key derives from them) or `admin:ManageCertificates` with a provider can obtain certificates for any name of the provider's zones: scope the token/key/TSIG policy to the `_acme-challenge` records of the zones of the distributions |
| T/I | Main listener certificates | `--certs-dir` keystore with atomic reload (fsnotify + SIGHUP), files limited to 1 MiB, warning for keys readable by others, TLS ≥ 1.2 with AEAD/ECDHE suites (`internal/service/certs/keystore.go`, `internal/transport/http/server/tls.go`) | An unreadable file keeps the previous certificates (`error` log, metric `tls_server_certificate_reloads_total{result="error"}`) |

## 6. Domain cache (`internal/service/cache`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Cache poisoning / one user's response served to another | Unambiguous key that includes identity and variants (`cache/key.go`), configurable bypass for authenticated/cookie requests | A cache policy that ignores relevant headers is the administrator's choice |
| T | Altered on-disk entries | CRC of body and metadata, size and key check on every read (`cache/disk.go`); removals confined to the directory with `os.Root` | — |
| I | Cached copies of data readable by other local users | Entry files and webhook queue with `0600` permissions (`0750` directories) | The L2 tier holds the **plaintext** of the objects served by distributions, even if encrypted with SSE-S3/KMS on the drives: put the cache on an encrypted volume or do not enable L2 for sensitive content |
| D | Massive invalidation, slow webhooks | Persistent queues with backoff, concurrency limits, single flight per key (`flight.go`) | — |
| E | SSRF through purge webhooks | Egress policy on the address actually connected (after DNS resolution: rebinding is blocked too), no redirects or proxies, never the cluster nodes, private networks only if in `--cache-webhook-allow`; secrets only from the `NEXO_SECRET_WEBHOOK_*` namespace (`cache/egress.go`) | — |
| R | Unattributable purges | Invalidations with an id and audit | — |

## 7. Events and webhooks (`internal/service/event`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Impersonated recipient | Verified TLS for targets; `tls_skip_verify` is an explicit per-target opt-in (`event/factory.go`) | A target configured with `tls_skip_verify` accepts a MITM |
| T | SQL injection in the PostgreSQL target | Static statements with separate arguments; the simple protocol (client-side interpolation, GO-2026-5004 of pgx < 5.9.2) is replaced by the extended protocol (`event/dialers.go`) | See §10.1 for the pgx upgrade |
| I | Credentials in logs or errors | URLs redacted (user info and query) in errors and in target status (`event/webhook.go`); no redirects followed | — |
| D | Slow or unreachable target | Persistent queue (`queue_dir`) with a limit and retries (`event/queue.go`) | When the queue is full the target's new events are rejected (error logged) until the queue drains |
| E | Target as SSRF | Targets are defined only in the server configuration (`admin:ConfigUpdate`); bucket rules can only reference them by ARN; only `http(s)` endpoints | An administrator can point a target at internal services (a deliberate choice): the test of a target (`POST /notify/targets/{id}/test`) reports the HTTP status or the connection error, which reveals whether an internal port answers; `client_cert`/`client_key` name files of the node, whose absence the error reveals. Grant `admin:ConfigUpdate` only to who administers the nodes |

## 8. KMS and encryption (`internal/service/kms`, `internal/service/crypto`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Fake KES | `https` endpoints only, no redirects, client certificate (`kms/kes.go`) | Without a client certificate KES accepts anyone holding the API key (warning at startup) |
| T | Altered sealed keys | AES-GCM to seal the object key with a KEK derived via HKDF from a random salt per sealing (`crypto/crypto.go`); authenticated DARE stream with anti-truncation (`crypto/stream.go`); part keys derived injectively (`crypto/key.go`, part number ≤ 2³²−1) | — |
| I | Exposed master key | Master key only from env/file (`NEXO_KMS_SECRET_KEY`), never in logs (redacted types); file keystore `0600` in a `0700` directory (`kms/keystore.go`); IAM secrets encrypted at rest with Argon2id + AES-GCM (`internal/service/iam/crypto.go`) | Whoever reads the process environment reads the builtin KMS master key: prefer KES in production |
| D | KMS unreachable | Explicit errors, no plaintext fallback | SSE-KMS objects are unreadable until the KMS comes back |
| E | Another client's SSE-C key | The SSE-C key is not stored; verification with a salted HMAC (`crypto.go` `ssecHMAC`) | — |

## 9. `nexoctl` CLI (`internal/cli`)

| STRIDE | Threat | Mitigation (code) | Residual risk |
|--------|----------|----------------------|-----------------|
| S | Fake server | TLS verification always on (system CAs), no option to disable it | An `http://` alias sends requests in plain text (signed, but readable) |
| T | Zip-slip / writing outside the destination | `JoinTarget` rejects paths that leave the directory (`internal/cli/location.go`), atomic writes (`transfer.go`) | — |
| I | Readable alias credentials | Configuration file `0600` in a `0700` directory, warning if more permissive; configuration exports (rules, distributions, cache) written `0600`; pprof profiles written `0600` | Credentials are in plain text in the user's configuration file (like `mc`, `aws`) |
| E | Commands executed by the CLI | Only the program the user passes explicitly (`find --exec`, `internal/cli/commands/find.go`, `deps.go`) | — |
| T | Out-of-range numbers in JSON documents | The `int32` values of the documents (lifecycle days, `MaxAgeSeconds`) are saturated instead of truncated (`s3sdk_config.go` `intPtr32`): `2^32+1` days do not become 1 day | — |

## 10. Automated scans (`make security`, CI job `security`)

`make security` runs, with versions pinned in the Makefile and in the workflow:

- `govulncheck@v1.8.0 ./...`: known vulnerabilities **reached by the code** (modules and standard library of the
  `go.mod` toolchain; v1.8.0 is the first release whose `x/tools` analyzes Go 1.27 code);
- `gosec@v2.29.0 -exclude-dir=web ./...`: insecure code patterns; every false positive carries a
  `// #nosec Gxxx -- reason` next to it (gosec ignores `//nolint`);
- `npm audit --omit=dev --audit-level=moderate` in `web/`: production dependencies of the dashboard.

### 10.1 govulncheck

Upgrades applied in M9 (compatible with `go 1.24`): `github.com/go-chi/chi/v5` v5.2.2 (open redirect GO-2025-3770),
`github.com/rabbitmq/amqp091-go` v1.13.0 (eight vulnerabilities of the AMQP client: DoS, OOM, overflow, TLS without a
minimum version, exposed credentials), `github.com/aws/aws-sdk-go-v2/service/s3` v1.97.3 and
`aws/protocol/eventstream` v1.7.8 (EventStream decoder panic GO-2026-5764).

Mitigations in the code, introduced in M9 for vulnerabilities whose fixes required a newer toolchain:

| Vulnerability | Reachability | Mitigation |
|---------------|-----------------|-------------|
| GO-2026-5970 (`x/text/unicode/norm`, infinite loop on invalid UTF-8) | `match.toNFC` normalizes client-controlled keys | Invalid sequences no longer reach `norm`: only the valid runs are normalized (`internal/service/match/normalize.go`, test `TestNormalizeInvalidUTF8`, fuzzing with a watchdog `FuzzNormalize`) |
| GO-2026-5004 (pgx, SQL injection with dollar-quoted literals in the simple protocol) | `postgresConn.Exec` | Extended protocol forced (`event/dialers.go`); the statements are static |
| GO-2026-6629 (`x/text/secure/precis`, panic) | only via `pgx.Connect` (SCRAM on the configured password) | Administrator input, not client input |

**Toolchain upgraded with v1.0** (final integration): the vulnerabilities of the Go 1.24 standard library
(`net/http`, `crypto/tls`, `crypto/x509`, `encoding/xml`, `encoding/asn1`, `net/url`, `net/textproto`, `os`) and those
of `golang.org/x/net` (HTTP/2: GO-2026-6617, -6612, -6611, -6610, -6603, -4918, -4559; idna GO-2026-5026),
`golang.org/x/text`, pgx, `go.opentelemetry.io/otel` and `google.golang.org/grpc` are closed by moving to:

```sh
go get go@1.26.9 golang.org/x/net@v0.60.0 golang.org/x/text@v0.42.0 github.com/jackc/pgx/v5@v5.9.2 \
  go.opentelemetry.io/otel@v1.45.0 go.opentelemetry.io/otel/sdk@v1.45.0 \
  go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.45.0 \
  go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.45.0 \
  google.golang.org/grpc@v1.83.2 github.com/klauspost/compress@v1.18.7 github.com/go-chi/chi/v5@v5.3.0
go mod tidy
```

(`x/net` v0.60.0 requires `x/text` v0.42.0; grpc v1.83.2, `klauspost/compress` v1.18.7 and chi v5.3.0 close
GO-2026-6443, GO-2026-5841 and GO-2026-5774/5775/5777/4316, present in the modules but not reached.) CI uses
golangci-lint v2.14.0 (2.5.0 is built with Go 1.25.1 and rejects `go 1.26` modules) and the Dockerfiles use
`GO_VERSION=1.26.9`. With `x/net` v0.60.0 the HTTP/2 transport of `x/net/http2` and `h2c` are deprecated: the
internode RPC client uses the HTTP/2 of `net/http` (`http.Transport` with HTTP/2 or cleartext HTTP/2 `Protocols` and
`HTTP2Config`, `internal/transport/rpc/conn.go`), as the server already did (`http.Server.Protocols`).

`govulncheck@v1.1.4 ./...` (run with the `go.mod` toolchain, `GOTOOLCHAIN=go1.26.9` locally): **no vulnerabilities
reached**; only GO-2026-5932 in `golang.org/x/crypto` remains (no fixed version, not reached). The mitigations in the
table above stay in the code as defense in depth.

**GO-2026-5932 cannot be dropped** (checked in October 2026 with `govulncheck@v1.8.0`, `go mod why -m`, `go mod
graph`, `go list -deps`): the notice is about the package `golang.org/x/crypto/openpgp`, unmaintained and part of
every version of the `golang.org/x/crypto` module, so govulncheck reports it at the module level ("Module Results")
whenever the module is required, and no version fixes it. Nexo requires the module directly (`x/crypto/acme` for
the ACME certificates, `x/crypto/argon2` for the key of the IAM file store, `x/crypto/chacha20poly1305` for the
object encryption) and through `github.com/nats-io/nkeys` (`nacl/box`), `google.golang.org/grpc` and
`golang.org/x/net`; none of these imports `openpgp`, which is never compiled into the binaries (`go list -deps` of
every build tag finds no `x/crypto/openpgp`). Dropping the module would mean replacing the ACME client, Argon2id,
ChaCha20-Poly1305 and the NATS client, for a package that is not built: the notice is accepted and re-checked at
every dependency upgrade.

### 10.2 gosec

150 initial findings (all High/Medium except one). Fixes:

| Rule | Fix |
|--------|-----------|
| G115 | Weight of cache entries saturated at `MaxUint32` (`cache/store.go`); part number limited to 32 bits before key derivation (`crypto/key.go`); CLI `int32` values saturated (`cli/s3sdk_config.go`) |
| G122 | Removals during the cache scan through `os.Root` (`cache/disk.go`) |
| G302/G306 | Cache entries and purge webhook queue `0600`; CLI exports `0600` (`access.go`, `cache.go`, `dist.go`) |

The false positives (138 `#nosec` lines, each with its reason) are: G101 on names of headers, parameters, environment
variables and IAM actions (21); G115 on conversions with verified bounds or intentional bit reinterpretations (54);
G304 on files chosen by the operator or by the CLI user, or on paths already resolved and verified by the storage
(26); G401/G501/G505 on MD5/SHA1 mandated by the S3 protocol (ETag, Content-MD5, MD5 of the SSE-C key,
`x-amz-checksum-sha1`), never used as security primitives (18); G404 on retry jitter (3); G407 on the zero nonce of
the sealing, safe because the KEK is derived from a random salt for every sealing and encrypts a single message; G402
on the opt-in `tls_skip_verify`; G124 on cookies (`Secure` follows the connection); G117, G118, G204, G301, G602, G705
justified case by case. After the fixes gosec reports only one G117 in the code of the M8 audit service
(`internal/service/audit/audit.go`, field `AccessKey`: the public identifier of the key, the same false positive
annotated in `share.go`), to be annotated with its own `#nosec`.

### 10.3 npm audit

`npm audit --omit=dev` in `web/`: 0 vulnerabilities.

## 11. Fuzzing (`make fuzz`, nightly CI job `fuzz-nightly`)

`make fuzz` finds every `func FuzzXxx(f *testing.F)` and runs each target for `FUZZTIME` (default 60s,
`FUZZ=regexp` to select them); a crash leaves the input in the package's `testdata/fuzz/<Target>/`, to be committed
with the fix as a regression test.

| Surface | Target |
|-----------|--------|
| S3 XML parsers (all bodies), bucket JSON policies | `test/fuzz` `FuzzS3Bodies` (end-to-end on an in-process server, SigV4-signed as root: versioning, tagging, CORS, lifecycle, notifications, object lock, retention, legal hold, encryption, multi-delete, complete multipart, create bucket, policy) |
| Admin API JSON | `test/fuzz` `FuzzAdminJSON` (45 main operations: buckets, IAM, policies, access rules, distributions, key groups, certificates, cache, shares, presign) |
| SigV4 (header, query, chunked, POST policy) | `internal/service/auth` `FuzzParseAuthorization`, `FuzzParseQuery`, `FuzzVerifyAuthorization`, `FuzzVerifyPresignedQuery`, `FuzzChunkedBody`, `FuzzPostPolicyDocument` |
| JSON policies | `internal/service/policy` `FuzzParse`, `FuzzConditionValues` |
| xl.meta | `internal/service/storage` `FuzzDecodeXLMeta`; bitrot `internal/service/erasure` `FuzzBitrotReader` |
| Endpoints and ellipses | `internal/domain` `FuzzParseEndpoint`, `internal/service/topology` `FuzzExpandArg` |
| Match (glob/regex/normalization) | `internal/service/match` `FuzzNormalize`, `FuzzCompileAndMatch` |
| Distribution signatures and paths | `internal/service/distribution` `FuzzVerifySignedRequest`, `FuzzWebsitePath`, `FuzzResolve` |
| Lifecycle, CORS, notification XML | `internal/service/ilm` `FuzzParseLifecycle`, `internal/service/bucket` `FuzzParseCORS`, `internal/service/event` `FuzzParseBucketConfig` |
| Range header | `internal/domain` `FuzzParseRange` |
| CLI targets | `internal/cli` `FuzzTargetParser` |

The oracles go beyond the absence of panics: S3-typed errors only, idempotent normalization, round trip of the
notification configuration and of endpoints, consistency of the CORS rules' `Match`, no 5xx response (except 501)
from the server.

M9 run (60 s per target, 2 workers, 4 vCPUs): 24 targets, from 80 thousand (end-to-end) to 1.4 million executions
each. A single defect found: `ParseEndpoint("0 /")` accepted the directory `"0 "`, which, reread after the arguments
were trimmed, became `"0"` (another drive); now a path that starts or ends with spaces is rejected
(`internal/domain/endpoint.go`, corpus in `internal/domain/testdata/fuzz/FuzzParseEndpoint/`).

## 12. Consolidated residual risks and recommendations

The penetration test of the console and of the Admin API (October 2026, T-9.3) is reported in
[pentest-2026-10.md](pentest-2026-10.md): the issues it confirmed are fixed (PT-01..PT-04), the accepted ones are
listed there with their rationale and summarized in §3 and §7.

1. **Toolchain**: with v1.0, Go 1.26.9 and the upgraded modules (§10.1), `govulncheck` reports no reached
   vulnerabilities; every release must rerun `make security` and update the toolchain to Go's patch releases.
2. **Rate limits off by default and per node**: the token buckets of the S3 API, of the Admin API and of the
   distributions (`ratelimit.*`, `api.requests_max`, `restrictions.rateLimit`) must be configured, and each node
   counts its own requests (no cluster-wide counter); they bound requests, not bandwidth or connections: keep a
   reverse proxy with connection limits in front of public deployments (configurations in `deploy/proxy`).
3. **Optional TLS**: without TLS, console cookies, signatures replayable within the skew window and internode traffic
   are exposed. In production: `--certs-dir` on all listeners and `--tls-internode-verify`.
4. **Root credentials = cluster key**: whoever knows them signs internode tokens and sessions; rotate them with the
   procedure in docs/07 and do not reuse them across deployments (the deployment id separates the derived keys
   anyway).
5. **Plaintext in the cache's L2 tier** for encrypted objects served by distributions (§6).
6. **Builtin KMS master key in the process environment** (§8): in production, KES with a client certificate.
7. **Peer CAs for the RPC clients read at startup** (§4): a new CA requires a coordinated restart.
8. **pprof profiles**: they contain addresses, stacks and memory fragments; grant `admin:Profiling` only to those
   who administer the nodes.
9. **Slow or stalled clients**: every write of a response must progress within `NEXO_SEND_TIMEOUT` (20 s), after
   which the response is aborted, and an HTTP/2 connection that accepts no bytes for the same time is closed with all
   its streams. On HTTP/2 writes progress in pieces sized on the connection's streams, so a client that steadily
   reads at least about 40 KB/s per connection is not interrupted, whatever the number of streams; slower than that
   (or below about 1.6 KB/s on HTTP/1) it may be. Until then an unauthenticated client that does not read holds a
   handler, the request body already received (at most the connection's HTTP/2 window: 1 MiB, 16 MiB on the S3
   listener of a cluster) and, for reads of more than 8 parts, the object's read lock. The streams of authenticated
   RPC calls (`httpx.LiftSendBound`) follow the pace of the downstream S3 client and have no per-stream limit, only
   the connection's (a node always reads its own socket). The number of connections per client must be limited by
   the reverse proxy (item 2).
