# Trace, logs and audit

| Tool | What it shows | Where |
|------|---------------|-------|
| Live trace | every completed request, of every node | `nexoctl admin trace`, console **Monitoring → Trace** |
| Live logs | the server logs of every node | `nexoctl admin logs`, console **Monitoring → Logs** |
| Audit | one record per authenticated request: who, what, when, outcome | JSON log, in-memory search, webhook and Kafka targets |
| OpenTelemetry | distributed spans per request and per call between nodes | an OTLP collector |

In a cluster, trace, logs and audit are **aggregated from every node**: connecting to any one of them is enough.

## Live trace

```console
$ nexoctl admin trace local
Tracing the requests of "local" (Ctrl-C to stop).
2026-10-15 11:51:14.442 local s3    ListObjectsV2  200  0.47ms  GET /photos?delimiter=%2F&list-type=2  rx 0B tx 460B  user admin  from 127.0.0.1
2026-10-15 11:51:14.452 local s3    GetObject      200  0.69ms  GET /photos/hello.txt?x-id=GetObject  rx 0B tx 9B  user admin  from 127.0.0.1
2026-10-15 11:51:14.462 local s3    HeadObject     404  0.36ms  HEAD /photos/missing  user admin  from 127.0.0.1  error: Not Found
```

One line per request: time, node, type (`s3`, `admin`, `web`, `rpc`), operation, status, duration, method and
path, bytes received and sent, user, client, error. Useful filters:

```bash
nexoctl admin trace --errors local                                  # only status >= 400
nexoctl admin trace --status 5xx,403 local
nexoctl admin trace --api PutObject,GetObject --bucket photos local
nexoctl admin trace --min-duration 500ms local                      # only slow requests
nexoctl admin trace --node nexo2.lan:9000 local                     # a single node
nexoctl admin trace --type s3,admin,rpc local                       # calls between nodes too
nexoctl admin trace --timeout 1m --json local > trace.jsonl         # JSON Lines, for 1 minute
```

- Request headers and bodies are never published; signatures, credentials, tokens and passwords in the query
  are redacted, together with the usual credential names of the sites served by the distributions (`apikey`,
  `api_key`, `api-key`, `x-api-key`, `auth`, `sig`, `jwt`, `session`, `sid`). The other application parameters
  stay in clear text: do not pass secrets in parameters with other names.
- With no clients connected the cost is zero; a slow client loses events (reported as `dropped`) but never
  slows down the requests.
- The stream reconnects by itself if the server restarts; it ends with Ctrl-C, `--timeout` or `--count`.
- A node serves at most 256 live trace streams and 256 live log streams at once (the console tabs, the CLI
  and the streams the other nodes open for them): beyond them a new stream is refused with 429
  `too_many_requests` until one closes.
- Requires `admin:ServerTrace` (root, `consoleAdmin`, `diagnostics`).

## Live logs

The server logs go to stderr (JSON by default, `--log-format text` for plain text; with systemd they end up
in the journal, with Docker in `docker logs`). To follow them remotely, aggregated from every node:

```bash
nexoctl admin logs local                          # the last 10 lines of every node, then the new ones
nexoctl admin logs --level warn --last 100 local
nexoctl admin logs --node nexo2.lan:9000 --json local
```

Level and format are changed **at runtime** on the whole deployment, without a restart:

```bash
nexoctl admin config set local logger level=debug
nexoctl admin config reset local logger          # back to --log-level / --log-format
```

Passwords, tokens, signatures, cookies and SSE-C keys are redacted from the logs. Requires `admin:ConsoleLog`.

## Audit log

Every S3 and console request produces an audit record with identity (access key and user), API,
bucket and object, client IP, outcome, latency, bytes, request id and node. The exceptions are the anonymous
health probes (`/nexo/health/*` without credentials) and the successful anonymous reads of the metrics
(`/nexo/metrics/*` with `NEXO_PROMETHEUS_AUTH_TYPE=public`): a load balancer or the Kubernetes probes repeat
them every few seconds and would fill the in-memory ring, evicting the useful records. Still recorded are an
authenticated scrape (JWT), a rejected scrape (missing, expired or forged token) and every request that
presents credentials (SigV4 or presigned signature, token), even towards `/nexo/health/*` and even if rejected:
an attempt to try out credentials always leaves a trace. The records:

1. end up in the server log (`"msg":"audit"`, component `audit`; `time` is the time of the record,
   `event_time` that of the request) when `NEXO_AUDIT_LOG=on` (`--audit-log`) or at the `debug` log
   level. Off by default: one log line per request costs throughput, a lot on a Windows console;
2. stay in memory on every node (the last 10000) for searching from the console (**Monitoring →
   Audit**) and from the CLI;
3. are delivered to the configured **audit targets**, with a persistent on-disk queue if the target does
   not answer.

### Search

```console
$ nexoctl admin audit --limit 5 local
TIME                    STATUS  DURATION  USER     API            RESOURCE
2026-10-15 11:51:14.464 200       0.29ms  admin    ListObjectsV2  photos
2026-10-15 11:51:14.462 404       0.37ms  admin    HeadObject     photos/missing  error: Not Found
2026-10-15 11:51:14.452 200       0.72ms  admin    GetObject      photos/hello.txt
```

```bash
nexoctl admin audit --user alice --since 1h local
nexoctl admin audit --action DeleteObject --bucket photos --since 2026-10-15T09:00:00Z local
nexoctl admin audit --errors --status 403 --limit 1000 --json local
nexoctl admin audit --request-id 8F1B7F3FB3152251 local
```

The search covers only the recent records in memory: for long-term retention configure a target.

### Audit targets (webhook and Kafka)

The targets are subsystems of the dynamic configuration, applied at runtime. The webhook receives one
JSON document per record (POST):

```bash
nexoctl admin config set local audit_webhook \
  enable=on endpoint=https://siem.example.com/nexo auth_token=s3cr3t     # sent as "Authorization: Bearer s3cr3t"
```

Kafka receives one message per record, with key `<node>/<request id>`:

```bash
nexoctl admin config set local audit_kafka \
  enable=on brokers=kafka1:9092,kafka2:9092 topic=nexo-audit \
  tls=on sasl=on sasl_mechanism=sha512 sasl_username=nexo sasl_password='…'
```

Common keys: `queue_dir` (subdirectory of the persistent queue, relative to the queues root on the
first local drive; absolute paths and `..` are rejected), `queue_limit` (default 100000 records),
`comment`. For several targets of the same type use an id: `audit_webhook:siem`, `audit_webhook:backup`. The
credentials are masked in `admin config get` and in the logs.

## OpenTelemetry

With an OTLP collector, every S3, console, distribution or inter-node request produces a server span, and
every call between nodes a client span that propagates the context (W3C `traceparent`). The attributes
contain no secrets (method, path without query, status, API, bucket, request id, client).

```bash
NEXO_OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 \
NEXO_OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
NEXO_OTEL_TRACES_SAMPLER_ARG=0.1 \
nexo server /srv/nexo
```

| Variable | Default | Notes |
|----------|---------|-------|
| `NEXO_OTEL_EXPORTER_OTLP_ENDPOINT` | — (disabled) | base URL for `http/protobuf` (`/v1/traces` appended if missing), `host:port` for `grpc` |
| `NEXO_OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` | or `grpc` |
| `NEXO_OTEL_TRACES_SAMPLER_ARG` | `1` | fraction of the traces started by the node; a request with a sampled `traceparent` is always recorded |

`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT` and `OTEL_EXPORTER_OTLP_CERTIFICATE` apply too.
When disabled, tracing has no cost. With Helm: `config.otel.*` in `values.yaml`.
