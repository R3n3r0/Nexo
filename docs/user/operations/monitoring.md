# Monitoring

## Health checks

The health endpoints answer without authentication on **both** ports (9000 and 9001), `GET` or
`HEAD`, with `200` when everything is fine and `503` otherwise:

| Endpoint | Meaning | Use |
|----------|---------|-----|
| `/nexo/health/live` | the process is alive | liveness probe, automatic restarts |
| `/nexo/health/ready` | the node can serve traffic (storage initialized, bootstrap complete, quorum) | readiness probe, load balancer health check |
| `/nexo/health/cluster` | status of the whole deployment | dashboards, maintenance scripts |

```console
$ curl -s http://localhost:9000/nexo/health/cluster
{"ok":true,"status":"ok","checks":{"storage":"ok"},"nodes":4,"nodesOnline":4,"drives":16,"drivesOnline":16,"writeQuorum":true,"maintenance":true}
```

- `writeQuorum`: every erasure set accepts writes.
- `maintenance`: **another** node can be shut down without losing the quorum (check it before a
  restart).
- A node that is shutting down answers `503` to `/ready` too, so the load balancer takes it out of rotation
  before it closes its connections.

`nexo healthcheck` (used by the `HEALTHCHECK` of the images) probes the local node without `curl`:
`nexo healthcheck --ready` exits with 0 when the node is ready; with `NEXO_CERTS_DIR` (or `--certs-dir`) it
uses HTTPS, trusting the certificates in the directory.

`nexoctl admin info local` summarizes version, mode, pools, sets (with their status and quorum), nodes
(online, uptime, clock skew, healing) and drives (usage and status); `nexoctl admin usage local`
shows the usage per bucket.

## Prometheus metrics

The metrics are on the S3 port, in three endpoints:

| Endpoint | Contents | Scrape |
|----------|----------|--------|
| `/nexo/metrics/v1/node` | the families of the node that answers (label `server`) | every node |
| `/nexo/metrics/v1/cluster` | the families of **every** node (collected through RPC) plus the `nexo_cluster_*` families | a single target (or the load balancer) |
| `/nexo/metrics/v1/bucket` | per-bucket metrics summed over every node and bucket usage | one target, every 60 s |

### Authentication

With `NEXO_PROMETHEUS_AUTH_TYPE=jwt` (default) the routes require `Authorization: Bearer <JWT>` (without a
token: `403`). The token is signed with the secret key of an identity that has `admin:Prometheus` (root,
`consoleAdmin` or the `diagnostics` policy): a dedicated user is advisable.

```bash
nexoctl admin user add local prometheus 'a-long-random-password' --policy diagnostics
nexoctl alias set metrics http://nexo.lan:9000 prometheus 'a-long-random-password'

nexoctl admin prometheus generate metrics             > scrape-cluster.yml   # job nexo-cluster
nexoctl admin prometheus generate --expiry 365d metrics node    # job nexo-node
nexoctl admin prometheus generate metrics bucket      # job nexo-bucket
```

Output (to paste into `scrape_configs` of `prometheus.yml`):

```yaml
# Nexo cluster metrics of nexoctl alias "metrics" (access key prometheus), token valid until 2027-10-15T11:50:08Z
scrape_configs:
  - job_name: "nexo-cluster"
    metrics_path: "/nexo/metrics/v1/cluster"
    scheme: http
    authorization:
      type: Bearer
      credentials: "eyJhbGciOiJIUzUxMiIs…"
    static_configs:
      - targets: ["nexo.lan:9000"]
```

The token expires after `--expiry` (default `365d`) or when the user's secret key changes; treat it like the
key itself. With `NEXO_PROMETHEUS_AUTH_TYPE=public` the endpoints are public and
`nexoctl admin prometheus generate --public` produces the configuration without a token: use it only if port
9000 is not reachable from untrusted networks.

### Main families

| Area | Metrics |
|------|---------|
| S3 API | `nexo_s3_requests_total{api,status}`, `nexo_s3_requests_4xx_errors_total{api}`, `nexo_s3_requests_5xx_errors_total{api}`, `nexo_s3_request_duration_seconds{api}`, `nexo_s3_requests_inflight`, `nexo_s3_traffic_received_bytes_total`, `nexo_s3_traffic_sent_bytes_total` |
| Buckets | `nexo_bucket_requests_total{bucket,api,status_class}`, `nexo_bucket_traffic_received_bytes_total{bucket}`, `nexo_bucket_traffic_sent_bytes_total{bucket}`, `nexo_bucket_usage_total_bytes`, `nexo_bucket_objects`, `nexo_bucket_versions`, `nexo_bucket_delete_markers` |
| Drives | `nexo_drive_online`, `nexo_drive_healing`, `nexo_drive_total_bytes`, `nexo_drive_used_bytes`, `nexo_drive_free_bytes`, `nexo_drive_free_inodes`, `nexo_drive_operations_total{drive,op}`, `nexo_drive_operation_seconds_total{drive,op}`, `nexo_drive_errors_total{drive,type}` |
| Cluster | `nexo_cluster_nodes_online`, `nexo_cluster_nodes_offline`, `nexo_cluster_drives_{online,offline,healing,total}`, `nexo_cluster_sets_{healthy,degraded,read_only,unavailable}{pool}`, `nexo_cluster_read_quorum{pool}`, `nexo_cluster_write_quorum{pool}`, `nexo_cluster_buckets`, `nexo_cluster_objects`, `nexo_cluster_usage_bytes` |
| Healing and scanner | `nexo_mrf_queue_pending`, `nexo_mrf_queue_healed_total`, `nexo_heal_jobs{state}`, `nexo_heal_drive_*{drive}`, `nexo_scanner_cycles_total`, `nexo_ilm_actions_total{action}` |
| Notifications | `nexo_events_pending{target}`, `nexo_events_target_online{target}`, `nexo_events_sent_total{target}`, `nexo_events_failed_total{target}` |
| Network, locks, KMS | `nexo_rpc_requests_total{peer,method}`, `nexo_rpc_errors_total{peer,method}`, `nexo_rpc_peer_online{peer}`, `nexo_lock_acquire_timeouts_total{type}`, `nexo_kms_online`, `nexo_kms_errors_total{op,reason}` |
| TLS | `nexo_tls_server_certificate_expiry_seconds{certificate}`, `nexo_tls_server_certificate_reloads_total{result}` |
| Process | `nexo_node_uptime_seconds`, `nexo_node_info{version,commit}`, `go_*`, `process_*` |

The cardinality is bounded: no object keys in the labels, at most 1000 buckets per node (beyond that:
`_other`). The complete list is in [docs/07 §7.4](https://github.com/R3n3r0/Nexo/blob/main/docs/07-security-operations.md#74-observability).

## Ready-made Grafana and Prometheus

[`deploy/docker/observability`](https://github.com/R3n3r0/Nexo/tree/main/deploy/docker/observability)
contains the Prometheus configuration (`prometheus.single.yml`, `prometheus.cluster.yml`), the alerting
rules (`alerts.yml`) and the Grafana dashboard *Nexo / Overview* with a preconfigured datasource. With Compose:

```bash
cd deploy/docker
# single node
docker compose -f compose.single.yml -f compose.observability.yml up -d
# cluster
PROMETHEUS_CONFIG=prometheus.cluster.yml \
  docker compose -f compose.cluster.yml -f compose.observability.yml up -d
```

Prometheus is at <http://localhost:9090>, Grafana at <http://localhost:3000> (`admin`/`admin`, to be changed
with `GRAFANA_ADMIN_*`). The Prometheus files expect `NEXO_PROMETHEUS_AUTH_TYPE=public` in `nexo.env`;
otherwise uncomment the `authorization` block and mount the token. On Kubernetes the chart creates a
ServiceMonitor (`serviceMonitor.enabled`, token in `serviceMonitor.bearerTokenSecret`).

To import the dashboard into an existing Grafana use
`observability/grafana/dashboards/nexo-overview.json`.

## Alerts

`alerts.yml` (validated in CI against the real metrics) defines:

| Alert | When |
|-------|------|
| `NexoNodeDown` | a scrape of the node has been failing for 1 minute |
| `NexoClusterNodesOffline` | the cluster sees offline nodes |
| `NexoDriveOffline` | a drive has been offline for 2 minutes |
| `NexoErasureSetDegraded` | a set has had missing drives for 5 minutes |
| `NexoQuorumAtRisk` | a set has lost the write quorum or is close to losing it |
| `NexoKMSOffline` | the KMS does not answer (SSE-S3/SSE-KMS do not work) |
| `NexoCapacityHigh` | usable capacity of the cluster above 85% |
| `NexoDriveAlmostFull`, `NexoDriveInodesLow` | a drive almost full or out of inodes |
| `NexoHigh5xxRate` | too many 5xx responses |
| `NexoSlowRequests` | high p99 latency for 15 minutes |
| `NexoInternodeErrors`, `NexoLockTimeouts` | errors in the calls between nodes, lock timeouts |
| `NexoHealingStalled`, `NexoDriveHealingStalled` | healing queue or repopulation of a drive stuck |
| `NexoEventQueueBacklog`, `NexoEventTargetDown` | queued notifications or unreachable target |

Every rule has `severity` among its labels and `summary`/`description` among its annotations; for the
diagnosis see [Troubleshooting](troubleshooting.md).

## Console charts

The **Monitoring → Metrics** page of the console shows requests, errors, latencies (p50, p95, p99),
bandwidth, drives, healing, scanner, calls between nodes, CPU and memory, for the cluster or per node, with
a time range and automatic refresh. The data comes from an in-memory buffer on every node (one point every
10 s for 24 h), available without Prometheus too:

```console
$ nexoctl admin metrics list local
NAME                         UNIT         AGG   DESCRIPTION
s3_requests                  req/s        sum   S3 requests per second
s3_errors_5xx                req/s        sum   S3 requests answered with a 5xx status per second
s3_latency_p99               seconds      max   99th percentile of the latency of the S3 requests
…
$ nexoctl admin metrics query --range 6h --step 5m local s3_latency_p99
```

They require `admin:Prometheus` or `admin:ServerInfo`. The buffer is reset when the node restarts: for
history use Prometheus.

## Diagnostics

- `nexoctl admin speedtest local drive|object|net`: measures the drives, the object layer (temporary bucket
  `nexo-speedtest-*`) and the network between the nodes (cluster only). It loads the server: run it
  off-peak.
- `nexoctl admin profile --type cpu --seconds 30 --output cpu.pb.gz local`: pprof profile of the node that
  answers (`cpu`, `heap`, `goroutine`, `mutex`, `block`), to open with `go tool pprof`. No
  `/debug/pprof` endpoint is exposed; a profile reveals internal details, treat it as confidential data.

Both require dedicated permissions (`admin:Speedtest`, `admin:Profiling`), granted to root and to the
`diagnostics` policy.
