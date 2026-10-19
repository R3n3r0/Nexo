# Configuration

Nexo has two configuration levels:

- **static**: read at startup from flags, `NEXO_*` environment variables and an optional JSON file; changing
  it requires a restart;
- **dynamic**: key=value subsystems saved in the deployment (on every node), changeable at runtime
  from the console (**Configuration**), from `nexoctl admin config` or from the Admin API (`/api/v1/config`).

## Static configuration

Precedence: **flags > environment variables > `--config` file > defaults**. Every flag of `nexo server` has
its own `NEXO_<FLAG>` variable (dashes → underscores, upper case), with the exceptions noted (`--config` is
`NEXO_CONFIG_FILE`, `--dry-run` has no variable). List values accept several comma-separated entries in
the variables, and repeated or comma-separated flags on the command line.

### Credentials and endpoints

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `NEXO_ROOT_USER` / `NEXO_ROOT_USER_FILE` | — | required | Root user (≥ 3 characters). Only from the environment or a file, never from a flag |
| `NEXO_ROOT_PASSWORD` / `NEXO_ROOT_PASSWORD_FILE` | — | required | Root password (≥ 8 characters). The `_FILE` variant wins over the plain variable |
| `NEXO_ENDPOINTS` | arguments of `nexo server` | — | Drives and nodes, separated by spaces (the command-line arguments win). See [Deployment modes](deployment-modes.md) |
| `NEXO_CONFIG_FILE` | `--config` | — | JSON configuration file (lowest precedence) |

### Network and public URLs

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `NEXO_ADDRESS` | `--address` | `:9000` | S3 API listener (also health, metrics and RPC between nodes) |
| `NEXO_CONSOLE_ADDRESS` | `--console-address` | `:9001` | Console and Admin API listener |
| `NEXO_REGION` | `--region` | `us-east-1` | S3 region reported to clients and used in signatures |
| `NEXO_DOMAIN` | `--domain` | — | Enables virtual-host requests (`<bucket>.<domain>`) |
| `NEXO_SERVER_URL` | `--server-url` | — | Public S3 URL used in presigned and share links (behind a proxy) |
| `NEXO_BROWSER_REDIRECT_URL` | `--browser-redirect-url` | — | Public URL of the console |
| `NEXO_TRUSTED_PROXIES` | `--trusted-proxies` | — | IPs or CIDRs of the trusted reverse proxies |
| `NEXO_FORWARDED_HEADERS` | `--forwarded-headers` | `x-forwarded` | Header family read from trusted proxies: `x-forwarded` (`X-Forwarded-For/Proto/Host`) or `forwarded` (RFC 7239) |
| `NEXO_TRUST_FORWARDED_HOST` | `--trust-forwarded-host` | `false` | Uses the forwarded host instead of the `Host` header (only for proxies that rewrite it) |
| `NEXO_PROXY_PROTOCOL` | `--proxy-protocol` | — | Listeners that accept PROXY protocol v1/v2 from trusted proxies: `s3`, `console`, `web` |
| `NEXO_WEB_ADDRESS` | `--web-address` | — | HTTP listener that serves only the distributions and the ACME HTTP-01 challenges (e.g. `:80`) |
| `NEXO_WEB_TLS_ADDRESS` | `--web-tls-address` | — | TLS listener for the distributions only, with their certificates by SNI (e.g. `:443`) |
| `NEXO_UNKNOWN_HOST` | `--unknown-host` | `path-style` | Response of the S3 listener to an unknown host: `path-style` (S3 API) or `misdirected` (421) |
| `NEXO_S3_DISTRIBUTIONS` | `--s3-distributions` | `auto` | Distributions served by the S3 listener too: `auto` (only if `NEXO_SERVER_URL` or `NEXO_DOMAIN` declare the names of the S3 endpoint), `on` (explicit opt-in), `off` (web listeners only) |
| `NEXO_NO_DISTRIBUTIONS` | `--no-distributions` | `false` | Recovery: no host routed to a distribution on any listener (they can still be changed from the Admin API) |

### TLS

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `NEXO_CERTS_DIR` | `--certs-dir` | — (HTTP) | Directory with `public.crt` and `private.key` (enables TLS on every listener), hot-reloaded |
| `NEXO_TLS_MIN_VERSION` | `--tls-min-version` | `1.2` | Minimum TLS version: `1.2` or `1.3` |
| `NEXO_TLS_INTERNODE_VERIFY` | `--tls-internode-verify` | `false` | mTLS between nodes (`https://` endpoints) |

Details in [TLS and certificates](security/tls.md).

### Storage and erasure coding

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `NEXO_METADATA_LOG` | `--metadata-log` | `false` | Keeps the metadata of the objects in a segmented log with group commit on each drive (faster small writes); drives are converted at start, and back when disabled. See [Metadata log](operations/metadata-log.md) |
| `NEXO_STORAGE_CLASS_STANDARD` | `--storage-class-standard` | automatic | Parity of the `STANDARD` class as `EC:n` (SNMD and cluster) |
| `NEXO_STORAGE_CLASS_RRS` | `--storage-class-rrs` | `EC:1` | Parity of the `REDUCED_REDUNDANCY` class (at most that of `STANDARD`) |

### Access and distributions

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `NEXO_ACCESS_RULES_MAX` | `--access-rules-max` | `200` | Maximum number of access rules per bucket |
| `NEXO_KEY_NORMALIZATION` | `--key-normalization` | `nfc` | Unicode normalization of the keys matched by the rules: `nfc` or `none` |
| `NEXO_BLOCK_PUBLIC_ACCESS` | `--block-public-access` | `false` | Cluster-wide block public access (until it is changed at runtime) |
| `NEXO_BLOCK_PUBLIC_ACCESS_MODE` | `--block-public-access-mode` | `reject` | `reject` (public rules rejected when saved) or `ignore` (ignored during evaluation) |
| `NEXO_SECRET_<NAME>` / `NEXO_SECRET_<NAME>_FILE` | — | — | Secrets referenced as `${secret:<name>}` by access rules, origin secrets and webhooks (lower-case name, `-` and `_` equivalent) |
| `NEXO_ACME_DIRECTORY` | `--acme-directory` | Let's Encrypt | ACME directory for automatic certificates |
| `NEXO_ACME_EMAIL` | `--acme-email` | — | Contact of the ACME account |
| `NEXO_ACME_CHALLENGE` | `--acme-challenge` | `http-01` | `http-01` (on the web listener) or `tls-alpn-01` (on the web TLS listener) |
| `NEXO_ACME_CA_CERT` | `--acme-ca-cert` | — | CA (PEM) of a private ACME server |
| `NEXO_ACME_DNS_NAMESERVERS` | `--acme-dns-nameservers` | — | nameservers (`host[:port]`, comma-separated) the DNS-01 propagation check queries instead of the authoritative ones of the zone (split-horizon DNS) |

### Distributions cache

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `NEXO_CACHE_DIR` | `--cache-dir` | — (memory only) | Directory of the disk tier (SSD/NVMe separate from the data drives) |
| `NEXO_CACHE_SIZE` | `--cache-size` | `10GiB` | Size of the disk tier (of the memory tier without `--cache-dir`) |
| `NEXO_CACHE_MEMORY` | `--cache-memory` | `256MiB` | Size of the memory tier |
| `NEXO_CACHE_MODE` | `--cache-mode` | `local` | In a cluster: `local`, `shared` or `shared+local` |
| `NEXO_CACHE_WEBHOOK_ALLOW` | `--cache-webhook-allow` | — | Internal networks reachable by the purge webhooks (public addresses only by default) |

Sizes accept `KiB`, `MiB`, `GiB`, `TiB` (and `KB`, `MB`, `GB`, `TB`).

### Encryption (KMS and SSE)

| Variable | Default | Description |
|----------|---------|-------------|
| `NEXO_KMS_SECRET_KEY` / `_FILE` | — | Master key of the built-in KMS: `<name>:<base64 of 32 bytes>` |
| `NEXO_KMS_SECRET_KEY_PREVIOUS` / `_FILE` | — | Replaced master keys (comma-separated), for rotation only |
| `NEXO_KMS_ENDPOINT` | — | `https://` URLs of the KES servers (comma-separated) |
| `NEXO_KMS_KEY_NAME` | — | Default key on KES |
| `NEXO_KMS_CLIENT_CERT` / `NEXO_KMS_CLIENT_KEY` | — | Client certificate and key for mTLS to KES |
| `NEXO_KMS_CAPATH` | system roots | CAs of the KES servers (file or directory) |
| `NEXO_SSEC_ALLOW_INSECURE` | `off` | Accepts SSE-C keys over plain HTTP: **testing only** |

Environment only (no flags). Details in [KMS and SSE](security/kms-sse.md).

### Logs, metrics and tracing

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `NEXO_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, `error` (changeable at runtime with the `logger` subsystem) |
| `NEXO_LOG_FORMAT` | `--log-format` | `json` | `json` or `text` |
| `NEXO_AUDIT_LOG` | `--audit-log` | `false` | Also write every audit record (one per request) to the server log; the `debug` level logs them anyway |
| `NEXO_PROMETHEUS_AUTH_TYPE` | — | `jwt` | `/nexo/metrics/v1/*` endpoints: `jwt` (bearer token) or `public` |
| `NEXO_OTEL_EXPORTER_OTLP_ENDPOINT` | — | — (disabled) | OpenTelemetry collector (`http(s)://…`) |
| `NEXO_OTEL_EXPORTER_OTLP_PROTOCOL` | — | `http/protobuf` | `http/protobuf` or `grpc` |
| `NEXO_OTEL_TRACES_SAMPLER_ARG` | — | `1` | Fraction (0–1) of the sampled traces |

The standard variables `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT` and
`OTEL_EXPORTER_OTLP_CERTIFICATE` apply too. See [Monitoring](operations/monitoring.md) and
[Trace, logs and audit](operations/trace-log-audit.md).

### Advanced cluster parameters (environment only)

| Variable | Default | Description |
|----------|---------|-------------|
| `NEXO_LOCK_STALE_AFTER` | `60s` | After how long a lock no longer refreshed by a node is released |
| `NEXO_LOCK_REFRESH_INTERVAL` | `10s` | Lock refresh interval (lower than `NEXO_LOCK_STALE_AFTER`) |
| `NEXO_RPC_CALL_TIMEOUT` | `30s` | Maximum duration of a call between nodes |
| `NEXO_HEAL_RETRY_MAX_DELAY` | `5m` | Maximum wait between two background healing attempts of an object |
| `NEXO_CACHE_RESYNC_INTERVAL` | `2m` | Periodic reload of the shared state (IAM, policies, configuration) on every node |
| `NEXO_SEND_TIMEOUT` | `20s` | After how long a response whose client stopped reading is aborted: S3 downloads and distribution sites, console and Admin API, on every listener; an HTTP/2 connection that accepts no bytes for the same time is closed together with all its streams |

### Configuration file

`--config nexo.json` (or `NEXO_CONFIG_FILE`) reads a JSON document with the camelCase keys of the flags;
missing keys keep their defaults. The root credentials, the secrets and the "environment only" parameters
cannot be put in the file.

```json
{
  "address": ":9000",
  "consoleAddress": ":9001",
  "region": "eu-south-1",
  "serverUrl": "https://s3.example.com",
  "browserRedirectUrl": "https://console.example.com",
  "certsDir": "/etc/nexo/certs",
  "trustedProxies": ["10.0.0.0/8"],
  "logLevel": "info",
  "logFormat": "json",
  "endpoints": ["/mnt/disk{1...4}"],
  "storageClassStandard": "EC:2",
  "cacheDir": "/var/cache/nexo",
  "cacheMode": "local"
}
```

### Validating without starting

`nexo server --dry-run` applies the same precedence, validates every value (credentials, URLs, CIDRs,
parity, KMS, cache…), the topology, the drive permissions, the ports and the certificates, and prints a
summary; on error it exits with a non-zero code and the list of **all** the problems found.

## Dynamic configuration

```bash
nexoctl admin config get local                       # all subsystems
nexoctl admin config get local logger api            # only some of them
nexoctl admin config set local logger level=debug    # the other keys are kept
nexoctl admin config reset local logger              # back to the defaults
nexoctl admin config export local --output config.json
nexoctl admin config import local config.json
```

Changes apply to the whole deployment (in a cluster they are propagated to every node). Sensitive values
(passwords, tokens, credentials in URLs) are shown masked; sending the masked value back keeps the
stored one. The complete schema, with the default of every key and whether the server applies it, is at
`GET /api/v1/config/schema` and on the **Configuration** page of the console.

| Subsystem | Keys | Applied at runtime |
|-----------|------|--------------------|
| `logger` | `level` (`debug`…`error`), `format` (`text`, `json`) | ✅ |
| `api` | `cors_allow_origin` (default `*`), `root_access` (default `on`; `off` denies the root credentials on the S3 API) | ✅ |
| `api` | `requests_max` (default `0`: no limit), `requests_deadline` (default `10s`): see [Rate limits](#rate-limits) | ✅ at the next request |
| `ratelimit` | `s3_ip`, `s3_access_key`, `s3_bucket`, `admin_ip`, `admin_user` and their `_burst`: see [Rate limits](#rate-limits) | ✅ at the next request |
| `scanner` | `speed` (`fastest`…`slowest`), `cycle` (default `1m`) | ✅ at the start of every cycle |
| `access` | `block_public`, `block_public_mode` (`reject`, `ignore`) | ✅ |
| `notify_webhook`, `notify_kafka`, `notify_nats`, `notify_amqp`, `notify_redis`, `notify_postgres` (also `:<id>`) | `enable`, target keys, `queue_dir`, `queue_limit`, `comment` | ✅ (see [Events](events-lifecycle.md)) |
| `audit_webhook`, `audit_kafka` (also `:<id>`) | like the `notify_*` of the same type | ✅ (see [Audit](operations/trace-log-audit.md#audit-log)) |
| `browser` | `session_duration` (12h), `upload_max_size` | ❌ not applied yet |
| `region` | `name`, `comment` | ❌ the effective region is the one of `--region` |

`nexoctl admin config set` warns when it sets a key that is not applied (`region.name is stored but not
applied yet by the server`); the console flags it the same way.

### Rate limits

Every node limits the requests it serves with token buckets: a key admits its rate in requests per second on
average and its `_burst` (default: the rate) at once. All of them are `0` by default: no limit.

| Key | Limits | Answer beyond it |
|-----|--------|------------------|
| `ratelimit.s3_ip` | S3 requests of a client address (the client behind the [trusted proxies](#network-and-public-urls); IPv6 by /64 network) | `503 SlowDown` (S3 XML) |
| `ratelimit.s3_access_key` | S3 requests of an access key (authenticated requests; presigned URLs count against the address and the bucket) | `503 SlowDown` |
| `ratelimit.s3_bucket` | S3 requests to a bucket, whoever sends them | `503 SlowDown` |
| `ratelimit.admin_ip` | Admin API requests of a client address (console and `nexoctl admin`) | `429 too_many_requests` (problem+json) |
| `ratelimit.admin_user` | Admin API requests of an authenticated user | `429 too_many_requests` |
| `api.requests_max` | S3 requests a node serves at once; a request beyond it waits `api.requests_deadline` for a slot | `503 SlowDown` |

Every refusal carries `Retry-After` (seconds): the S3 SDKs retry `SlowDown` with backoff. The health probes
(`/nexo/health/*`), the metrics scrapes (`/nexo/metrics/*`) and the internode RPC are never limited; the
[distributions](distributions/README.md#rate-limit) have their own limit. The refused requests are counted in
`nexo_ratelimit_throttled_total{api,kind}`, and marked in the audit log (tag `throttled` with the kind of the
limit) and in the live trace (`error: Service Unavailable (rate limit: ip)`).

```bash
nexoctl admin config set local ratelimit s3_ip=200 s3_ip_burst=400 admin_ip=20
nexoctl admin config set local api requests_max=1024 requests_deadline=5s
nexoctl admin config reset local ratelimit
```

**In a cluster the limits are per node.** The configuration is the same on every node (a change reaches all of
them at once), but each node counts its own requests: behind a load balancer spreading the clients over N nodes
the deployment admits up to N times each limit (a client sending to a single node meets the limit of that
node). Divide the limit you want for the deployment by the number of nodes. This keeps the check local (no
internode call per request) and keeps working when nodes fail.
