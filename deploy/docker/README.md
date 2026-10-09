# Nexo with Docker and Docker Compose

Everything here uses the same image, `ghcr.io/r3n3r0/nexo` (server + `nexoctl`, distroless, non-root
UID 65532). The deployment mode is chosen by the arguments of `nexo server`, not by the image
(see [docs/08-modalita-deploy.md](../../docs/08-modalita-deploy.md)).

| File | Purpose |
|------|---------|
| `compose.single.yml` | SNSD: one container, one named volume |
| `compose.snmd.yml` | SNMD: one container, 4 volumes, erasure coding (`server /data{1...4}`) |
| `compose.cluster.yml` + `nginx.conf` | Cluster 4 nodes × 2 drives behind an nginx load balancer (9000 S3, 9001 console) |
| `compose.cluster-tls.yml` + `nginx-tls.conf` | Same cluster with TLS: `./certs` mounted at `/certs`, nginx in TCP passthrough |
| `compose.observability.yml` | Prometheus + Grafana overlay with a provisioned Nexo dashboard |
| `nexo.env.example` | Template for `nexo.env` (root credentials and options) |
| `gen-dev-certs.sh` | Development CA and certificate for the TLS cluster |
| `Dockerfile.goreleaser`, `Dockerfile.nexoctl.goreleaser` | Release images built by GoReleaser from prebuilt binaries |
| `Dockerfile` | Symlink to the root `Dockerfile` (used by `make docker`) |

## Images

```bash
# From the repository root. The web console is built inside the image.
docker build -t nexo .
docker build -f Dockerfile.nexoctl -t nexoctl .

# Multi-arch with buildx
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=$(git describe --tags --always) \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t ghcr.io/r3n3r0/nexo:dev .
```

Image facts: `EXPOSE 9000 9001`, `VOLUME /data`, `USER nonroot`, `HEALTHCHECK` with `nexo healthcheck`
(distroless has no `curl`), `ENTRYPOINT ["/nexo"]`, `CMD ["server", "/data"]`.

## Single node

```bash
cd deploy/docker
cp nexo.env.example nexo.env            # set NEXO_ROOT_USER / NEXO_ROOT_PASSWORD
docker compose -f compose.single.yml up -d
docker compose -f compose.single.yml ps # healthy within ~10 s
```

- Console: <http://localhost:9001>, S3 API: <http://localhost:9000>.
- Multi drive: `docker compose -f compose.snmd.yml up -d` (replace the named volumes with one bind
  mount per disk for real hardware).
- Ports can be changed with `NEXO_S3_PORT` / `NEXO_CONSOLE_PORT`, the image with `NEXO_IMAGE`
  (shell or a `.env` file next to the compose file).

Plain `docker run`:

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersecret \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo server /data
```

### Secrets

`NEXO_ROOT_USER_FILE` and `NEXO_ROOT_PASSWORD_FILE` read the credentials from files (Docker/Swarm
secrets); the `*_FILE` variable wins over the plain one.

### Host volumes and permissions

The container runs as UID 65532. A bind-mounted directory must be writable by that user:

```bash
sudo mkdir -p /srv/nexo && sudo chown 65532:65532 /srv/nexo
docker run ... -v /srv/nexo:/data ghcr.io/r3n3r0/nexo server /data
```

Named volumes inherit the ownership of `/data` from the image and need nothing.

## Cluster

```bash
cp nexo.env.example nexo.env
docker compose -f compose.cluster.yml up -d
docker compose -f compose.cluster.yml ps
```

Four nodes (`nexo1`..`nexo4`, two drives each) start the same command,
`server --console-address :9001 http://nexo{1...4}:9000/data{1...2}`, and form one erasure set.
Clients talk to the `lb` service (nginx) on 9000 and 9001. `nginx.conf`:

- forwards `Host` unchanged (SigV4 signatures include it, presigned URLs keep working);
- `client_max_body_size 0`, `proxy_request_buffering off`: uploads stream through;
- sets `X-Forwarded-For/Proto/Host/Port` (trust them with `NEXO_TRUSTED_PROXIES`);
- re-resolves node names through Docker DNS (`resolve`, nginx ≥ 1.27.3) and fails over on 502/503/504.

Stopping one node (`docker compose -f compose.cluster.yml stop nexo3`) keeps the cluster serving;
healing runs when it comes back.

A node serves the internode RPC (`/nexo/rpc/v1/`) on its S3 port from the start, but answers the
S3 and console requests with `503` (and its readiness probe `/nexo/health/ready` too) until the
cluster bootstrap is complete: a quorum of the nodes and of the drives of every erasure set must be
reachable, so the nodes can start in any order. A node started with other endpoints than its peers
refuses to join and exits. `nexo server --dry-run <endpoints>` checks the topology, the recognition of
the node among the endpoints (its host names or addresses and the port of `--address`), the drive
directories, the ports and the certificates without starting anything.

**Internode RPC security.** The internode calls are HTTP/2 only and carry a short-lived token
(HMAC derived from the root credentials and the deployment id, bound to the node it is sent to).
With `http://` endpoints (this compose file) that traffic, tokens included, travels in cleartext:
keep it on a trusted internal network (the compose network here) and use `https://` endpoints
(`compose.cluster-tls.yml`) anywhere else; a node logs a warning at startup when it runs without TLS.
The public proxy never forwards `/nexo/rpc/` (`nginx.conf` answers `404`): only the nodes talk to
each other on that path.

**Expansion** (docs/03 §3.8): add a second pool by restarting every node with both pools, e.g.
`server --console-address :9001 http://nexo{1...4}:9000/data{1...2} http://nexo{5...8}:9000/data{1...4}`
(every argument with an ellipsis is a pool). The objects of the first pool stay where they are; the new
objects go to the pools weighted by their free space.

### Cluster with TLS

```bash
./gen-dev-certs.sh s3.example.com        # or copy your own certificates into ./certs
docker compose -f compose.cluster-tls.yml up -d
curl --cacert certs/CAs/ca.crt https://localhost:9001/nexo/health/live
```

`./certs` is mounted at `/certs` on every node (`NEXO_CERTS_DIR=/certs`) and must contain `public.crt`,
`private.key` and `CAs/ca.crt`. The certificate needs SANs for `nexo1`..`nexo4` (internode calls),
`localhost` (healthcheck) and the public hostname. nginx (`nginx-tls.conf`) balances TCP connections
without terminating TLS, so the certificate the client sees is Nexo's and presigned URLs stay valid.
To terminate TLS at the edge instead, see [deploy/proxy](../proxy/).

## Observability

```bash
# overlay on a single node
docker compose -f compose.single.yml -f compose.observability.yml up -d
# overlay on the cluster
PROMETHEUS_CONFIG=prometheus.cluster.yml \
  docker compose -f compose.cluster.yml -f compose.observability.yml up -d
```

- Prometheus: <http://localhost:9090>, config in `observability/prometheus.{single,cluster}.yml`,
  example alerts in `observability/alerts.yml`.
- Grafana: <http://localhost:3000> (`admin`/`admin`, change with `GRAFANA_ADMIN_*`), datasource and the
  dashboard *Nexo / Overview* are provisioned from `observability/grafana/`.
- Nexo exposes metrics on the S3 port at `/nexo/metrics/v1/{cluster,node,bucket}`. Set
  `NEXO_PROMETHEUS_AUTH_TYPE=public` in `nexo.env` or configure a bearer token in the Prometheus files.

## CLI from a container

```bash
docker run --rm -it --network host \
  -v nexoctl-config:/home/nonroot/.nexoctl \
  ghcr.io/r3n3r0/nexoctl alias set local http://localhost:9000 admin supersecret
docker run --rm -it --network host -v nexoctl-config:/home/nonroot/.nexoctl ghcr.io/r3n3r0/nexoctl ls local

# or from the server image
docker exec -it nexo /nexoctl version
```

## Troubleshooting

| Symptom | Check |
|---------|-------|
| container `unhealthy` | `docker logs nexo`: missing/short root credentials (user ≥ 3, password ≥ 8 chars) |
| `permission denied` on `/data` | bind mount not owned by UID 65532 (see above) |
| presigned URL returns `SignatureDoesNotMatch` behind a proxy | the proxy rewrites `Host`; forward it unchanged |
| cluster node refuses to start | topology differs from `format.json` on its drives; use the same endpoints on every node |
