# 7. Security and operations

## 7.1 Threat model (STRIDE summary)

| Threat | Surface | Mitigation |
|----------|-----------|-------------|
| Spoofing | S3 API, Admin API, internode | SigV4 with constant-time comparison, short-lived internode JWT, optional mTLS between nodes |
| Tampering | Data on disk, traffic | Per-block bitrot hash, TLS 1.2+ (default 1.3), SSE |
| Repudiation | Administrative operations | Immutable audit log to an external target |
| Information disclosure | Console, errors, logs | No secrets in logs, generic error messages for anonymous users, IAM secrets encrypted at rest |
| Denial of service | Upload, listing, login | Header/body size limits, timeouts, login rate limit, per-node token buckets per client address, access key/user, bucket and distribution (`ratelimit.*`, `restrictions.rateLimit`), S3 concurrency limit (`api.requests_max`/`api.requests_deadline`): 503 SlowDown / 429 with Retry-After; health, metrics and internode RPC exempt |
| Elevation of privilege | Policy engine, console | Deny-by-default, authorization always server-side, systematic negative tests |

The full analysis per surface (S3, Admin API and console, internode RPC, distributions, cache, events, KMS, CLI),
with the mitigations in the code, the residual risks, the scans (`make security`: govulncheck, gosec, npm audit) and
the fuzzing targets (`make fuzz`), is in [docs/security/threat-model.md](security/threat-model.md).

## 7.2 Secret management
- Root credentials only from env/file (`NEXO_ROOT_USER`, `NEXO_ROOT_PASSWORD` or `_FILE`), never stored in plain text.
- IAM and config encrypted in `.nexo.sys` with a derived key (Argon2id/HKDF) or one provided by the KMS.
- Users' secret keys are shown only once in the console/CLI.
- Credentials of the ACME DNS-01 providers (Cloudflare API token, Route 53 secret access key and session token,
  RFC 2136 TSIG secret) are sealed with AES-256-GCM in the system store (`.nexo.sys`, the key of the distribution
  certificates derived from the root credentials and the deployment id, the provider name as additional data),
  write-only in the Admin API (`/dns-providers` returns `hasCredentials` only; an update with empty credentials
  keeps them), never logged nor formatted (`certs.DNSCredentials` and `nexoadmin.DNSCredentials` print
  `[REDACTED]` with `%v`/`%+v`/`%#v` and `slog`) and never written in a provider error. Route 53 can use the
  ambient credentials of the nodes (instance role, web identity) instead: nothing stored. Give the providers the
  narrowest permission (Cloudflare `Zone:DNS:Edit` on one zone; Route 53 `ChangeResourceRecordSets` on one hosted
  zone; a TSIG key allowed to update only `_acme-challenge.*` TXT records with `update-policy`).
- CLI config file with 0600 permissions and a warning if more permissive.
- No keys in logs: the SSE-C key (`domain.SSECustomerKey`), the SSE requests (`domain.SSERequest`), the KMS master
  keys (`kms.MasterKey`), the KMS data keys (`kms.DEK`) and the plaintext object keys (`crypto.ObjectKey`) are
  printed (`%v`, `%+v`, `%#v`), logged (`slog`) and JSON-encoded only in redacted form (MD5 of the SSE-C key, name
  and fingerprint of the master key, key id and size of the sealed form of a DEK, encryption type and cipher of an
  object key). Credentials in a webhook URL (user info, query: `?token=...`), and in every configuration value
  given as a URL (e.g. the NATS or Redis address `nats://user:pass@host`, `redis://:pass@host`), are masked in
  `GET /config` like the sensitive values (sending the masked value back keeps the stored one) and stripped from
  delivery errors, logs and target status; the KES endpoint (`NEXO_KMS_ENDPOINT`) appears in startup errors without
  user info or query.
- Webhooks **do not follow redirects**: a 3xx response is a failed delivery (`endpoint returned 3xx`). The recipient
  of a webhook (often a third party) therefore cannot bounce Nexo's POST, with the body and the target's
  `auth_token`, to internal hosts that no administrator configured (`127.0.0.1`, `169.254.169.254`: SSRF); an HTTP
  client injected into the target factory (`event.FactoryDeps.HTTP`) is used with the same policy.
- The `queue_dir` of notification targets is a path **relative** to the queue root
  (`<first local drive>/.nexo.sys/events/queue_dir/<queue_dir>`): absolute paths and `..` are rejected, because the
  queue consumes and removes the files in its directory.

### KMS and server-side encryption (SSE)
- **Builtin KMS** (`NEXO_KMS_SECRET_KEY="<name>:<base64 of 32 bytes>"`): the master key seals the data keys of
  SSE-S3 and SSE-KMS (the keys created with `nexoctl admin kms key create` are derived from it; the name registry
  lives in the system documents). The plaintext content never appears in the data on disk; the checksums
  (`x-amz-checksum-*`) are sealed with a key derived from the object key. For **SSE-C and SSE-KMS** the ETag is
  opaque too: the drives contain no digest of the plaintext content. For **SSE-S3**, as in S3, the ETag (of a PUT and
  of each part) is the MD5 of the plaintext content, returned to clients and therefore stored in plain text in the
  version metadata: SSE-S3 protects against drive theft, but the MD5 lets one confirm a guess about the content (and,
  for an object of a few bytes, even recover it); for data that must not reveal any digest, use SSE-KMS or SSE-C.
- **External KMS (KES)**: `NEXO_KMS_ENDPOINT` only accepts `https://` URLs (the data keys travel in plain text in the
  body of KES responses) and the client does not follow redirects; KES authenticates clients with **mTLS**
  (`NEXO_KMS_CLIENT_CERT`/`NEXO_KMS_CLIENT_KEY`, CA in `NEXO_KMS_CAPATH`): without a client certificate the server
  warns at startup.
- **SSE-C** requires TLS, as in S3: over plain HTTP the requests carrying the key are rejected
  (`400 InvalidRequest`). `NEXO_SSEC_ALLOW_INSECURE=on` accepts them: **for testing and development only**. With
  `nexoctl` the key should preferably be given with `NEXOCTL_ENC_C` (same syntax as `--enc-c`, comma-separated
  entries) or with a key file (`--enc-c-file`, one `ALIAS/BUCKET[/PREFIX]=BASE64KEY` entry per line, 0600
  permissions): in the arguments (`--enc-c`) it is readable by other local users (`/proc/<pid>/cmdline`) and stays in
  the shell history.
- **Cluster**: at startup the nodes compare the fingerprint of the KMS configuration (current and previous master
  keys, or KES endpoint and default key, never the secrets): a node whose KMS differs from the majority does not
  start (`ErrKMSMismatch`), because the objects encrypted by one node would be unreadable on the others.
- **Master key rotation** (builtin KMS):
  1. start all nodes with the new key in `NEXO_KMS_SECRET_KEY` and the previous one in
     `NEXO_KMS_SECRET_KEY_PREVIOUS` (several comma-separated keys): the data keys sealed by the previous key stay
     readable;
  2. reseal the data keys of all SSE-S3/SSE-KMS objects with the current key:
     `nexoctl admin kms reseal ALIAS [--bucket B]` (Admin API `POST /kms/reseal?bucket=`, action
     `admin:KMSCreateKey`); the data is not rewritten. The keys of in-progress multipart uploads are resealed too
     (their description contains the sealed object key: without it, after step 3 they would no longer accept parts
     or completion). The command reports the versions and uploads examined and the keys resealed and failed (it
     exits with an error if a key fails: run it again);
  3. once the command reports no errors on any bucket, remove `NEXO_KMS_SECRET_KEY_PREVIOUS` and restart.
  With KES, key rotation happens on the KES server; `reseal` moves the data keys to the current version.

## 7.3 TLS
- Certificates from `--certs-dir` (`NEXO_CERTS_DIR`; without it, the listeners serve plain HTTP), service
  `certs.Keystore` (`internal/service/certs/keystore.go`):
  - `public.crt` + `private.key`: default certificate (required; PEM chain leaf + intermediates, PKCS #8, PKCS #1
    or SEC 1 key, at most 1 MiB per file; a key readable by other users produces a warning);
  - `<name>/public.crt` + `<name>/private.key`: additional certificates chosen by **SNI** on the DNS names of the
    leaf (exact, then one-level wildcard; on a tie the valid one that expires last wins); hidden directories (`..data`
    of Kubernetes secrets) and those without a pair are ignored;
  - `CAs/*`: trusted CAs for peers. The peers' **client** certificates (`--tls-internode-verify`) are verified only
    with the CAs in `CAs/` and with the default certificate itself (a cluster sharing a single certificate),
    **never** with the intermediates of the `public.crt` chain: a CA that issues the server certificate does not
    become trusted for every certificate it has issued. The client certificate must also name (DNS or IP SAN) the
    host of one of the nodes' endpoints. The peers' **server** certificates are verified with the system roots,
    `CAs/` and the `public.crt` chain.
- **Hot reload without restart or interruption**: the directory (and those the symbolic links point to, such as
  certbot's `live/` and `archive/`) is watched with fsnotify and a 500 ms debounce, with a safety scan every minute;
  `SIGHUP` (`systemctl reload nexo`) also reloads the distributions' certificates immediately. Every handshake reads
  the current material: already open connections (HTTP/1.1 and HTTP/2) and in-flight responses continue with the
  certificate they started with. A failed reload (mismatched key, half-written file) keeps the previous certificates
  and is recorded (`nexo_tls_server_certificate_reloads_total{result}`); the expiry is exposed in
  `nexo_tls_server_certificate_expiry_seconds{certificate}` with a log warning at 21 days.
  `POST /service/restart` no longer uses `SIGHUP`: the restart is a request internal to the process.
- **Multiple SNI** on the S3 and console listeners and, as a fallback, on the distributions' TLS listener: a single
  `GetCertificate` chain = distribution certificates (only for their aliases) → SNI subdirectories → default.
- **Policy**: TLS 1.2 minimum (`--tls-min-version` / `NEXO_TLS_MIN_VERSION`: `1.2` or `1.3`), TLS 1.3 negotiated
  with every client that supports it; in TLS 1.2 only ECDHE suites with AEAD (AES-GCM, ChaCha20-Poly1305); ALPN
  `h2`, `http/1.1`.
- Option `--tls-internode-verify` for mTLS between nodes: the node presents the keystore's current certificate (even
  after a rotation) and verifies the peers' certificates with the current CAs in `CAs/` (or the shared certificate):
  the certificate must name one of the endpoint hosts, so a certificate from the same CA issued for another service
  does not pass. With node certificates issued by a private CA, put the CA in `CAs/`. Trust in the peers' servers
  (roots of the internode client) is fixed at startup: replacing the **CA** (or a self-signed certificate used as a
  root) requires a rolling restart of the nodes; renewing a leaf signed by the same CA does not.

## 7.4 Observability
- **Prometheus metrics** on `/nexo/metrics/v1/{node,cluster,bucket}` (S3 port, `NEXO_PROMETHEUS_AUTH_TYPE`):
  - `node`: the families of the responding node with the `server` label; `cluster`: the families of **every** node
    (collected via RPC, `server` label; an unreachable node is left out of the scrape) plus the `nexo_cluster_*`
    families computed from the deployment state; `bucket`: the `nexo_bucket_*` families summed over all nodes plus
    the scanner's per-bucket usage.
  - S3: `nexo_s3_requests_total{api,status}`, `nexo_s3_requests_{4xx,5xx}_errors_total{api}`,
    `nexo_s3_request_duration_seconds{api}`, `nexo_s3_requests_inflight`, `nexo_s3_traffic_{received,sent}_bytes_total`;
    bucket: `nexo_bucket_requests_total{bucket,api,status_class}`, `nexo_bucket_traffic_{received,sent}_bytes_total{bucket}`,
    `nexo_bucket_usage_total_bytes`, `nexo_bucket_{objects,versions,delete_markers}`.
  - Drives: `nexo_drive_{online,healing,total_bytes,used_bytes,free_bytes,used_inodes,free_inodes}{drive,pool,set}`,
    `nexo_drive_operations_total{drive,op}`, `nexo_drive_operation_seconds_total{drive,op}` (mean latency per
    operation), `nexo_drive_errors_total{drive,type}` (`offline`, `faulty`, `corrupt`, `full`, `timeout`, `format`, `other`).
  - Cluster: `nexo_cluster_nodes_{online,offline}`, `nexo_cluster_drives_{online,offline,healing,total}`,
    `nexo_cluster_sets_{healthy,degraded,read_only,unavailable}{pool}`, `nexo_cluster_{read,write}_quorum{pool}`,
    `nexo_cluster_capacity_{raw,usable}_*_bytes`, `nexo_cluster_{buckets,objects,usage_bytes}`.
  - Healing and scanner: `nexo_mrf_queue_{pending,leased}`, `nexo_mrf_queue_{added,merged,dropped,healed,retried}_total`,
    `nexo_heal_jobs{state}`, `nexo_heal_running_objects_*`, `nexo_heal_drive_*{drive}` (drives being repopulated),
    `nexo_scanner_{active,scanning,cycles_total}`, `nexo_scanner_last_cycle_*`, `nexo_ilm_actions_total{action}`,
    `nexo_ilm_{uploads_aborted,errors,versions_evaluated}_total`.
  - Notifications: `nexo_events_pending{target}`, `nexo_events_target_online{target}`, `nexo_events_{sent,failed}_total{target}`,
    `nexo_events_send_duration_seconds{target}`, `nexo_events_{published,dropped}_total`.
  - Internode, locks, KMS: `nexo_rpc_{requests,errors}_total{peer,method}`, `nexo_rpc_request_duration_seconds{peer,method}`,
    `nexo_rpc_peer_online{peer}`, `nexo_lock_acquire_duration_seconds{type}`, `nexo_lock_acquire_{timeouts,errors}_total{type}`,
    `nexo_kms_online`, `nexo_kms_requests_total{op}`, `nexo_kms_errors_total{op,reason}`, `nexo_kms_request_duration_seconds{op}`.
  - Cache and distributions: `nexo_cache_*{distribution,host,...}`, `nexo_distribution_*{distribution,...}`; process:
    `nexo_node_uptime_seconds`, `nexo_node_info{version,commit}`, `go_*`, `process_*`.
  - Bounded cardinality: no object key in labels; a bucket labels a series only if the request proves it exists
    (response < 400, server error or authenticated non-404 request), and at most 1000 buckets per node (beyond that:
    `_other`); the RPC methods are the fixed paths of the protocol.
- **Console series** (D-MON-01): in-memory ring buffer per node (one point every 10 s for 24 h) of the key series
  (`GET /api/v1/metrics` lists them: requests, errors, mean and p99 latency, traffic, drives, healing, notifications,
  RPC, CPU, memory); `GET /api/v1/metrics/query?metric=&range=&step=&end=&node=` returns the points of each node and
  the cluster aggregate (sum, mean or maximum depending on the series). Requires `admin:Prometheus` **or**
  `admin:ServerInfo`.
- **Live trace** (S-OPS-07, D-MON-02): `GET /api/v1/trace` (Server-Sent Events, `admin:ServerTrace`) publishes one S3,
  console, site or RPC request per event (time, node, API, method, path, **redacted** query — signatures,
  credentials, tokens, passwords, SSE-C, signed distribution URLs and the usual names of the application credentials
  of the served sites (`apikey`, `api_key`, `api-key`, `x-api-key`, `auth`, `sig`, `jwt`, `session`, `sid`); the
  other application parameters stay in clear —, status, duration, bytes, request id, IP, user, error) from all
  nodes, with the filters `node`, `type`, `api`, `bucket`, `status` (codes or classes such as `5xx`), `errors`,
  `threshold`. Headers and bodies are never published. Without subscribers the cost is zero; a slow client loses
  events (a `dropped` event with the counter, `nexo_trace_dropped_total`) without ever slowing down requests;
  heartbeat every 15 s. A node serves at most 256 live trace streams at once (`trace.DefaultMaxSubscribers`; the
  streams the peers open for their own subscribers count too): beyond them a new stream answers 429
  `too_many_requests`, so that an authorized client cannot exhaust the memory with open streams (pentest 2026-10,
  PT-03).
- **Live logs** (D-MON-02): `GET /api/v1/logs` (SSE, `admin:ConsoleLog`, filters `node`, `level`, `last` up to 10000
  entries per node resent on open). Logs stay JSON (or text) on stderr; level and format can be changed at runtime
  with the `logger` subsystem (`nexoctl admin config set logger level=debug format=text`; when unset,
  `--log-level`/`--log-format` apply). Secrets (Authorization, signatures, credentials, tokens, passwords, cookies,
  SSE-C keys) are redacted from the streams. As for the trace, a node serves at most 256 live log streams at once
  (`logs.DefaultMaxSubscribers`, 429 beyond them): each one buffers up to 10000 entries.
- **Audit** (S-OPS-06, D-MON-03): one record for every S3 and console request (identity, action/API, resource, IP,
  outcome, latency, request id, node, bytes; a successful console login records the user who signed in), except
  anonymous health probes (`/nexo/health/*` without credentials)
  and successful anonymous metrics scrapes (`/nexo/metrics/*` without identity or token, status < 400; a request with
  credentials, signature or token, and a rejected scrape, are always recorded). The record is written to the JSON
  log (`msg=audit`), kept in memory (the last 10000 per node, searchable with `GET /api/v1/audit` with
  `admin:ConsoleLog`, aggregated across nodes) and delivered to the `audit_webhook[:<id>]` and `audit_kafka[:<id>]`
  targets (same keys as the `notify_*` targets of the same type, persistent queue in
  `<first drive>/.nexo.sys/audit`, applied at runtime). The webhook receives one JSON document per record; Kafka a
  message with key `<node>/<request id>`.
- **Distributed tracing** (T-8.5): optional OpenTelemetry OTLP (`NEXO_OTEL_EXPORTER_OTLP_ENDPOINT`, protocol
  `http/protobuf` or `grpc`, sampling `NEXO_OTEL_TRACES_SAMPLER_ARG` with a parent-based decision): one server span
  per S3/console/site/RPC request that continues the caller's W3C `traceparent`, one client span for every internode
  call that propagates it. Attributes without secrets (method, path without query, status, API, bucket, request id,
  client). When disabled (the default) the cost is zero: the middlewares are the handler itself.
- **Alerts and dashboards** in `deploy/docker/observability/` (validated by `config_test.go` against the exposed
  families): node/drive offline, degraded sets, write quorum lost, KMS unreachable, capacity > 85%, drives nearly full
  or out of inodes, 5xx rate, p99 latency, internode errors, lock timeouts, stuck healing (MRF queue and drives),
  event queue and unreachable targets; Grafana dashboard `nexo-overview`.
- **Applied dynamic configuration** (`settings.Applied`): `logger.level`, `logger.format`, `audit_*`, `notify_*`,
  `api.root_access`, `api.cors_allow_origin`, `api.requests_max`, `api.requests_deadline`, `ratelimit.*`,
  `scanner.*`, `access.*`. The request limits (S-OPS-10) are per node, off by default; a cluster behind a load
  balancer admits up to the number of nodes times each limit (docs/user/configuration.md "Rate limits").

## 7.5 Deploy
| Target | Artifact |
|--------|-----------|
| Bare metal / VM | Static binaries (linux/darwin/windows, amd64/arm64), systemd unit, env file |
| Container | Signed multi-arch distroless image (cosign) + SBOM |
| Docker Compose | Single-node and 4-node examples |
| Kubernetes | Helm chart (StatefulSet, headless service, PVC per drive, PodDisruptionBudget, ServiceMonitor) |

### Operational recommendations
- Dedicated drives formatted with XFS, mounted with `noatime`, no RAID underneath Nexo.
- Same number and size of drives per node within a pool.
- NTP synchronized on all nodes.
- Load balancer in front of the nodes for S3 (9000) and console (9001) with a health check on `/nexo/health/ready`.

## 7.6 Main configuration

| Variable / flag | Default | Description |
|------------------|---------|-------------|
| `--address` / `NEXO_ADDRESS` | `:9000` | S3 API port |
| `--console-address` / `NEXO_CONSOLE_ADDRESS` | `:9001` | Console/Admin API port |
| `NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD` | — (required, min 8 characters) | Root credentials |
| `NEXO_REGION` | `us-east-1` | S3 region |
| `NEXO_DOMAIN` | — | Enables virtual-host style |
| `NEXO_STORAGE_CLASS_STANDARD` | auto (`EC:2..4`) | Default parity |
| `NEXO_KMS_SECRET_KEY` (or `_FILE`) | — | Master key of the builtin KMS, `<name>:<base64 of 32 bytes>`; the name is the default SSE-S3 key |
| `NEXO_KMS_SECRET_KEY_PREVIOUS` (or `_FILE`) | — | Replaced master keys (comma-separated): only to read the objects they sealed, until the `reseal` (§7.2) |
| `NEXO_KMS_ENDPOINT` | — | `https://` URLs of the KES servers, comma-separated (failover in order); `http://` is rejected |
| `NEXO_KMS_KEY_NAME` | — | Default key on KES (SSE-S3 and SSE-KMS without a key id) |
| `NEXO_KMS_CLIENT_CERT` / `NEXO_KMS_CLIENT_KEY` | — | Client certificate and key (PEM) for mTLS to KES |
| `NEXO_KMS_CAPATH` | system roots | CA (PEM file or directory) that signs the KES servers' certificates |
| `NEXO_SSEC_ALLOW_INSECURE` | `off` | Accepts SSE-C keys over plain HTTP: **testing and development only** |
| `api.cors_allow_origin` (config, `nexoctl admin config set`, applied at runtime) | `*` | CORS origins allowed for the whole deployment (comma-separated, `*` or empty: any origin): applies to service requests and to buckets **without** a CORS configuration, whose preflight answers 200 instead of S3's 403 (see [02 §2.4](02-features.md)); a bucket's CORS configuration replaces it for that bucket |
| `NEXO_PROMETHEUS_AUTH_TYPE` | `jwt` | `jwt` or `public`. With `jwt` the `/nexo/metrics/v1/*` routes require `Authorization: Bearer <JWT>`: HMAC (HS256/384/512) signed with the secret key of the `sub` (access key), `iss=prometheus`, `exp` required (the format of `mc admin prometheus generate`); the identity must have `admin:Prometheus` (root or the `diagnostics` policy). The token is never an S3 credential |
| `--certs-dir` / `NEXO_CERTS_DIR` | — (TLS disabled) | TLS certificates (§7.3), hot reloaded |
| `--tls-min-version` / `NEXO_TLS_MIN_VERSION` | `1.2` | Minimum TLS version accepted by the listeners (`1.2` or `1.3`) |
| `NEXO_OTEL_EXPORTER_OTLP_ENDPOINT` | — (tracing disabled) | `http(s)://` URL of the OTLP collector: base URL for `http/protobuf` (`/v1/traces` appended if the path is missing), host:port for `grpc`; `OTEL_EXPORTER_OTLP_HEADERS`, `_TIMEOUT`, `_CERTIFICATE` also apply |
| `NEXO_OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` | `http/protobuf` or `grpc` |
| `NEXO_OTEL_TRACES_SAMPLER_ARG` | `1` | Fraction (0-1) of the traces started by the node; a request with a sampled `traceparent` is always recorded |
| `logger.level` / `logger.format` (config, applied at runtime) | `--log-level` / `--log-format` | Log level and format |
| `audit_webhook[:<id>]` / `audit_kafka[:<id>]` (config, applied at runtime) | — | Audit log targets (keys of `notify_webhook`/`notify_kafka`) |

The console is always served on the console listener (`--console-address`, which also serves the Admin API):
there is no variable to disable it; restrict the exposure of the port instead (firewall, proxy). The duration of a
console session is fixed at 12 hours (not configurable); logout, secret rotation, removing the user and disabling the
user or the access key it was opened with revoke it earlier. A revocation by disable is for good: enabling the user or
the key again does not bring its sessions (nor its managed share links, revoked with them) back.

Every response of the console listener carries `X-Content-Type-Options: nosniff` and `X-Frame-Options: SAMEORIGIN`
(`DENY` and a `Content-Security-Policy` with `frame-ancestors 'none'` on the pages of the console), the answers of the
Admin API `Cache-Control: no-store` (unless the route sets its own policy, such as the downloads) and, when the
client came over TLS (directly or through a trusted proxy reporting it), `Strict-Transport-Security:
max-age=31536000` (`middleware.NewSecurityHeaders`, pentest 2026-10, PT-01 and PT-02). The penetration test of the
console and the Admin API is reported in [security/pentest-2026-10.md](security/pentest-2026-10.md).

## 7.7 Diagnostics (S-OPS-11)
- **pprof profiling** only through the Admin API (`POST /api/v1/profile`, `nexoctl admin profile`): no
  `/debug/pprof` endpoint is exposed on the listeners. Requires `admin:Profiling` (root and the `diagnostics` policy;
  `consoleAdmin` through `admin:*`). `cpu`, `mutex` and `block` sample for 1–120 s (default 10 s; mutex and block
  sampling is active only during the request), `heap` and `goroutine` are instantaneous; one profile at a time per
  node. A profile reveals function names and, for `goroutine`, the stacks: treat it as confidential data.
- **Speed tests** (`POST /api/v1/speedtest/{drive,object,net}`, `nexoctl admin speedtest`), `admin:Speedtest`:
  they write and then read for at most 60 s per phase, files/objects ≤ 64 MiB, ≤ 1 GiB per target, concurrency
  ≤ 64, one test at a time per node; the files live in the drives' temporary area (`.nexo.sys/tmp/speedtest-*`,
  removed at the end, otherwise by the janitor), the `object` test uses a temporary `nexo-speedtest-*` bucket
  removed at the end. They load the server: run them outside peak hours.
- In a cluster both act on the responding node (its console address); the `net` test measures the network to every
  other node through the internode RPC.
