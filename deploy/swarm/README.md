# Nexo on Docker Swarm

`stack.yml` deploys a cluster of 4 Nexo nodes × 2 drives (one erasure set of 8 drives) and two nginx
load balancers publishing the S3 API (`9000`) and the console (`9001`) on every Swarm node
(routing mesh). Same image and same command as `deploy/docker/compose.cluster.yml`.

| Element | Choice |
|---------|--------|
| Placement | `nexo1..nexo4` pinned to the Swarm nodes labelled `nexo.node=1..4`: their named volumes (local driver) never move |
| Discovery | Service names on the overlay network `nexo`, `endpoint_mode: dnsrr` (task address, no IPVS virtual IP and its idle timeout on long-lived internode connections), `hostname` = service name so that every node recognizes its own endpoints |
| Secrets | `nexo_root_user`, `nexo_root_password` (external Docker secrets) read through `NEXO_ROOT_USER_FILE` / `NEXO_ROOT_PASSWORD_FILE` |
| Network | Overlay with IPsec encryption between the Swarm nodes (`encrypted: "true"`), fixed subnet trusted for `X-Forwarded-*` |
| Updates | `docker service update` one node at a time (`parallelism: 1`, `stop-first`, 30 s apart): the erasure set keeps its quorum |
| Health | Image `HEALTHCHECK` (`nexo healthcheck`); nginx retries a read on another node while one restarts |

## Deploy

```bash
# 1. label the four Swarm nodes that hold the drives
docker node update --label-add nexo.node=1 swarm-a
docker node update --label-add nexo.node=2 swarm-b
docker node update --label-add nexo.node=3 swarm-c
docker node update --label-add nexo.node=4 swarm-d

# 2. root credentials as Docker secrets (password >= 8 characters)
printf admin | docker secret create nexo_root_user -
NEXO_PASSWORD="$(openssl rand -base64 30 | tr -d '\n')"   # keep it: a secret cannot be read back
printf %s "$NEXO_PASSWORD" | docker secret create nexo_root_password -

# 3. deploy (from deploy/swarm: the nginx configuration is ../docker/nginx.conf)
NEXO_TAG=v1.0.0 NEXO_SERVER_URL=https://s3.example.com docker stack deploy -c stack.yml nexo
docker stack services nexo
docker service logs -f nexo_nexo1
```

The four nodes start together and wait for each other (bootstrap); the load balancers answer `503`
until the cluster is ready. Then:

```bash
nexoctl alias set swarm http://<any-swarm-node>:9000 admin "$NEXO_PASSWORD"
nexoctl admin info swarm
```

## Variants

- **Drives on host disks** instead of named volumes: replace `nexoN-dataM:/dataM` with bind mounts
  (`/mnt/disk1:/data1`), owned by UID 65532 (`chown -R 65532:65532 /mnt/disk1`).
- **More nodes or drives**: add services `nexo5…` with their labels and volumes and change the
  endpoints of `command` (`http://nexo{1...8}:9000/data{1...2}`) on **every** service, then
  redeploy all of them together (a new pool: `http://nexo{1...4}:9000/data{1...2} http://nexo{5...8}:9000/data{1...2}`).
- **TLS between the nodes**: mount certificates (Docker secrets or configs) in `/certs`, set
  `NEXO_CERTS_DIR=/certs` and use `https://` endpoints; the overlay encryption can then be disabled.
  The image `HEALTHCHECK` follows `NEXO_CERTS_DIR`: it probes the console over HTTPS and trusts
  `/certs/public.crt` and `/certs/CAs/`, so the tasks stay healthy whatever names the certificate covers.
- **KMS**: `docker secret create nexo_kms_key` with `<name>:<base64 of 32 bytes>` and
  `NEXO_KMS_SECRET_KEY_FILE=/run/secrets/nexo_kms_key`.

## Media worker

`media-worker.yml` adds [nexo-media-worker](../../docs/user/media-worker.md) (renditions, posters,
thumbnails, image variants) to the same stack, with the target of Nexo configured for it:

```bash
# 1. the Swarm node of the worker (its job queue is a local volume): many cores, >= 4 GiB of memory
docker node update --label-add nexo.media=1 swarm-e

# 2. credentials of the worker and the token shared with Nexo
cp ../docker/media-worker.env.example media-worker.env
sed -i "s|^NEXO_WORKER_SECRET_KEY=.*|NEXO_WORKER_SECRET_KEY=$(openssl rand -hex 24)|; s|^NEXO_WORKER_AUTH_TOKEN=.*|NEXO_WORKER_AUTH_TOKEN=$(openssl rand -hex 32)|" media-worker.env

# 3. deploy both files
NEXO_TAG=v1.0.0 docker stack deploy -c stack.yml -c media-worker.yml nexo
docker service logs nexo_media-worker-setup     # "media-worker-setup: done: notify_media_worker -> ..."
```

| Service | Role |
|---------|------|
| `media-worker` | The worker (`paolor/nexo-media-worker`), one task, jobs endpoint `http://media-worker:9600/jobs` on the overlay network only; reads and writes through `http://lb:9000`; downloads OpenH264 into its volume at the first start (egress to `ciscobinary.openh264.org`) |
| `media-worker-tools` | Copies `nexoctl` out of the Nexo image (no shell) into the volume `media-worker-tools`, then exits |
| `media-worker-setup` | Runs the setup script of the Helm chart (`../helm/nexo/files/media-worker-setup.sh`) with busybox and the root secrets: least-privilege policy (`../docker/media-worker-policy.json`), user of the worker, `notify_media_worker` target with the token. Idempotent: change `media-worker.env` and deploy again |

`media-worker.env` is read by `docker stack deploy` (the worker takes its credentials from environment
variables, not from secret files): keep it readable by the deploying user only. Never run more than one
`media-worker` task: each one has its own job queue (see [Scaling and security](../../docs/user/media-worker.md#scaling-and-security)).

Validate the files without a Swarm: `docker stack config -c stack.yml` (CI job `deploy-files`), and
`docker stack config -c stack.yml -c media-worker.yml` with a `media-worker.env`.
