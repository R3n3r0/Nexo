# Monitoraggio

## Health check

Gli endpoint di salute rispondono senza autenticazione su **entrambe** le porte (9000 e 9001), `GET` o
`HEAD`, con `200` quando va tutto bene e `503` altrimenti:

| Endpoint | Significato | Uso |
|----------|-------------|-----|
| `/nexo/health/live` | il processo è vivo | liveness probe, restart automatici |
| `/nexo/health/ready` | il nodo può servire traffico (storage inizializzato, bootstrap completo, quorum) | readiness probe, health check del load balancer |
| `/nexo/health/cluster` | stato dell'intero deployment | dashboard, script di manutenzione |

```console
$ curl -s http://localhost:9000/nexo/health/cluster
{"ok":true,"status":"ok","checks":{"storage":"ok"},"nodes":4,"nodesOnline":4,"drives":16,"drivesOnline":16,"writeQuorum":true,"maintenance":true}
```

- `writeQuorum`: ogni erasure set accetta scritture.
- `maintenance`: si può spegnere **un altro** nodo senza perdere il quorum (da controllare prima di un
  riavvio).
- Un nodo che si sta fermando risponde `503` anche a `/ready`, così il load balancer lo toglie dal giro
  prima che chiuda le connessioni.

`nexo healthcheck` (usato dal `HEALTHCHECK` delle immagini) sonda il nodo locale senza `curl`:
`nexo healthcheck --ready` esce con 0 quando il nodo è pronto; con `NEXO_CERTS_DIR` (o `--certs-dir`) usa
HTTPS fidandosi dei certificati della directory.

`nexoctl admin info local` riassume versione, modalità, pool, set (con il loro stato e quorum), nodi
(online, uptime, sfasamento dell'orologio, healing) e dischi (uso e stato); `nexoctl admin usage local`
mostra l'uso per bucket.

## Metriche Prometheus

Le metriche sono sulla porta S3, in tre endpoint:

| Endpoint | Contenuto | Scrape |
|----------|-----------|--------|
| `/nexo/metrics/v1/node` | le famiglie del nodo che risponde (label `server`) | ogni nodo |
| `/nexo/metrics/v1/cluster` | le famiglie di **ogni** nodo (raccolte via RPC) più le famiglie `nexo_cluster_*` | un solo target (o il load balancer) |
| `/nexo/metrics/v1/bucket` | metriche per bucket sommate su tutti i nodi e uso dei bucket | un target, ogni 60 s |

### Autenticazione

Con `NEXO_PROMETHEUS_AUTH_TYPE=jwt` (default) le rotte richiedono `Authorization: Bearer <JWT>` (senza
token: `403`). Il token è firmato con la secret key di un'identità che ha `admin:Prometheus` (root,
`consoleAdmin` o la policy `diagnostics`): conviene un utente dedicato.

```bash
nexoctl admin user add local prometheus 'una-password-lunga-e-casuale' --policy diagnostics
nexoctl alias set metrics http://nexo.lan:9000 prometheus 'una-password-lunga-e-casuale'

nexoctl admin prometheus generate metrics             > scrape-cluster.yml   # job nexo-cluster
nexoctl admin prometheus generate --expiry 365d metrics node    # job nexo-node
nexoctl admin prometheus generate metrics bucket      # job nexo-bucket
```

Output (da incollare in `scrape_configs` di `prometheus.yml`):

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

Il token scade con `--expiry` (default `365d`) o quando cambia la secret key dell'utente; trattalo come la
chiave stessa. Con `NEXO_PROMETHEUS_AUTH_TYPE=public` gli endpoint sono pubblici e
`nexoctl admin prometheus generate --public` produce la configurazione senza token: usalo solo se la porta
9000 non è raggiungibile da reti non fidate.

### Famiglie principali

| Area | Metriche |
|------|----------|
| API S3 | `nexo_s3_requests_total{api,status}`, `nexo_s3_requests_4xx_errors_total{api}`, `nexo_s3_requests_5xx_errors_total{api}`, `nexo_s3_request_duration_seconds{api}`, `nexo_s3_requests_inflight`, `nexo_s3_traffic_received_bytes_total`, `nexo_s3_traffic_sent_bytes_total` |
| Bucket | `nexo_bucket_requests_total{bucket,api,status_class}`, `nexo_bucket_traffic_received_bytes_total{bucket}`, `nexo_bucket_traffic_sent_bytes_total{bucket}`, `nexo_bucket_usage_total_bytes`, `nexo_bucket_objects`, `nexo_bucket_versions`, `nexo_bucket_delete_markers` |
| Dischi | `nexo_drive_online`, `nexo_drive_healing`, `nexo_drive_total_bytes`, `nexo_drive_used_bytes`, `nexo_drive_free_bytes`, `nexo_drive_free_inodes`, `nexo_drive_operations_total{drive,op}`, `nexo_drive_operation_seconds_total{drive,op}`, `nexo_drive_errors_total{drive,type}` |
| Cluster | `nexo_cluster_nodes_online`, `nexo_cluster_nodes_offline`, `nexo_cluster_drives_{online,offline,healing,total}`, `nexo_cluster_sets_{healthy,degraded,read_only,unavailable}{pool}`, `nexo_cluster_read_quorum{pool}`, `nexo_cluster_write_quorum{pool}`, `nexo_cluster_buckets`, `nexo_cluster_objects`, `nexo_cluster_usage_bytes` |
| Healing e scanner | `nexo_mrf_queue_pending`, `nexo_mrf_queue_healed_total`, `nexo_heal_jobs{state}`, `nexo_heal_drive_*{drive}`, `nexo_scanner_cycles_total`, `nexo_ilm_actions_total{action}` |
| Notifiche | `nexo_events_pending{target}`, `nexo_events_target_online{target}`, `nexo_events_sent_total{target}`, `nexo_events_failed_total{target}` |
| Rete, lock, KMS | `nexo_rpc_requests_total{peer,method}`, `nexo_rpc_errors_total{peer,method}`, `nexo_rpc_peer_online{peer}`, `nexo_lock_acquire_timeouts_total{type}`, `nexo_kms_online`, `nexo_kms_errors_total{op,reason}` |
| TLS | `nexo_tls_server_certificate_expiry_seconds{certificate}`, `nexo_tls_server_certificate_reloads_total{result}` |
| Processo | `nexo_node_uptime_seconds`, `nexo_node_info{version,commit}`, `go_*`, `process_*` |

La cardinalità è limitata: nessuna chiave d'oggetto nelle label, al massimo 1000 bucket per nodo (oltre:
`_other`). L'elenco completo è in [docs/07 §7.4](https://github.com/R3n3r0/Nexo/blob/main/docs/07-security-operations.md#74-observability).

## Grafana e Prometheus pronti

[`deploy/docker/observability`](https://github.com/R3n3r0/Nexo/tree/main/deploy/docker/observability)
contiene la configurazione di Prometheus (`prometheus.single.yml`, `prometheus.cluster.yml`), le regole di
alert (`alerts.yml`) e la dashboard Grafana *Nexo / Overview* con datasource preconfigurato. Con Compose:

```bash
cd deploy/docker
# singolo nodo
docker compose -f compose.single.yml -f compose.observability.yml up -d
# cluster
PROMETHEUS_CONFIG=prometheus.cluster.yml \
  docker compose -f compose.cluster.yml -f compose.observability.yml up -d
```

Prometheus è su <http://localhost:9090>, Grafana su <http://localhost:3000> (`admin`/`admin`, da cambiare
con `GRAFANA_ADMIN_*`). I file Prometheus si aspettano `NEXO_PROMETHEUS_AUTH_TYPE=public` in `nexo.env`,
oppure decommenta il blocco `authorization` e monta il token. Su Kubernetes il chart crea un
ServiceMonitor (`serviceMonitor.enabled`, token in `serviceMonitor.bearerTokenSecret`).

Per importare la dashboard in un Grafana esistente usa
`observability/grafana/dashboards/nexo-overview.json`.

## Alert

`alerts.yml` (validato in CI contro le metriche reali) definisce:

| Alert | Quando |
|-------|--------|
| `NexoNodeDown` | uno scrape del nodo fallisce da 1 minuto |
| `NexoClusterNodesOffline` | il cluster vede nodi offline |
| `NexoDriveOffline` | un disco è offline da 2 minuti |
| `NexoErasureSetDegraded` | un set ha dischi mancanti da 5 minuti |
| `NexoQuorumAtRisk` | un set ha perso il quorum di scrittura o è vicino a perderlo |
| `NexoKMSOffline` | il KMS non risponde (SSE-S3/SSE-KMS non funzionano) |
| `NexoCapacityHigh` | capacità utile del cluster oltre l'85% |
| `NexoDriveAlmostFull`, `NexoDriveInodesLow` | un disco quasi pieno o senza inode |
| `NexoHigh5xxRate` | troppe risposte 5xx |
| `NexoSlowRequests` | latenza p99 alta per 15 minuti |
| `NexoInternodeErrors`, `NexoLockTimeouts` | errori nelle chiamate tra nodi, timeout dei lock |
| `NexoHealingStalled`, `NexoDriveHealingStalled` | coda di healing o ripopolamento di un disco fermi |
| `NexoEventQueueBacklog`, `NexoEventTargetDown` | notifiche in coda o target non raggiungibile |

Ogni regola ha `severity` tra le label e `summary`/`description` tra le annotazioni; per la diagnosi vedi
[Risoluzione dei problemi](troubleshooting.md).

## Grafici della console

La pagina **Monitoraggio → Metriche** della console mostra richieste, errori, latenze (p50, p95, p99),
banda, dischi, healing, scanner, chiamate tra nodi, CPU e memoria, per il cluster o per nodo, con intervallo
e aggiornamento automatico. I dati vengono da un buffer in memoria di ogni nodo (un punto ogni 10 s per
24 h), disponibile anche senza Prometheus:

```console
$ nexoctl admin metrics list local
NAME                         UNIT         AGG   DESCRIPTION
s3_requests                  req/s        sum   S3 requests per second
s3_errors_5xx                req/s        sum   S3 requests answered with a 5xx status per second
s3_latency_p99               seconds      max   99th percentile of the latency of the S3 requests
…
$ nexoctl admin metrics query --range 6h --step 5m local s3_latency_p99
```

Servono `admin:Prometheus` o `admin:ServerInfo`. Il buffer si azzera al riavvio del nodo: per lo storico
usa Prometheus.

## Diagnostica

- `nexoctl admin speedtest local drive|object|net`: misura i dischi, l'object layer (bucket temporaneo
  `nexo-speedtest-*`) e la rete tra i nodi (solo cluster). Carica il server: fuori dagli orari di punta.
- `nexoctl admin profile --type cpu --seconds 30 --output cpu.pb.gz local`: profilo pprof del nodo che
  risponde (`cpu`, `heap`, `goroutine`, `mutex`, `block`), da aprire con `go tool pprof`. Nessun endpoint
  `/debug/pprof` è esposto; un profilo rivela dettagli interni, trattalo come dato riservato.

Entrambi richiedono permessi dedicati (`admin:Speedtest`, `admin:Profiling`), concessi a root e alla
policy `diagnostics`.
