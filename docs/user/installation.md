# Installation

Nexo is distributed as two static binaries, `nexo` (server with the embedded web console) and `nexoctl`
(CLI), and as multi-arch Docker images. The mode (single drive, multiple drives, cluster) does not depend
on the package but on the arguments of `nexo server`: see [Deployment modes](deployment-modes.md).

| Platform | `nexo` | `nexoctl` |
|----------|--------|-----------|
| Linux amd64, arm64 | ✅ | ✅ |
| macOS amd64, arm64 | ✅ | ✅ |
| Windows amd64 | ✅ | ✅ |
| Windows arm64 | — | ✅ |

Every release contains archives, `.deb`/`.rpm` packages, a `checksums.txt` signed with cosign (keyless) and
an SPDX SBOM per archive. Verifying the signatures is described in
[docs/08 §8.7](https://github.com/R3n3r0/Nexo/blob/main/docs/08-deployment-modes.md#verifying-a-release).

## Binaries (archives)

```bash
VERSION=1.0.0
curl -LO https://github.com/R3n3r0/Nexo/releases/download/v$VERSION/nexo_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/R3n3r0/Nexo/releases/download/v$VERSION/nexoctl_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/R3n3r0/Nexo/releases/download/v$VERSION/checksums.txt
sha256sum --ignore-missing -c checksums.txt

tar xzf nexo_${VERSION}_linux_amd64.tar.gz
tar xzf nexoctl_${VERSION}_linux_amd64.tar.gz
sudo install -m 0755 nexo nexoctl /usr/local/bin/
nexo version
nexoctl --version
```

- The `nexo` archive also contains `systemd/nexo.service`, `systemd/nexo.env.example` and their README
  (see [systemd](#systemd)).
- The `nexoctl` archive contains the shell completions in `completions/`; they can also be
  generated with `nexoctl completion bash|zsh|fish|powershell` (see
  [`nexoctl completion`](cli/nexoctl_completion.md)).
- On Windows the archives are `.zip` files.

## deb and rpm packages

```bash
# Debian, Ubuntu
sudo apt install ./nexo_1.0.0_amd64.deb ./nexoctl_1.0.0_amd64.deb
# Fedora, RHEL, openSUSE
sudo dnf install ./nexo-1.0.0-1.x86_64.rpm ./nexoctl-1.0.0-1.x86_64.rpm
```

The `nexo` package:

- installs `/usr/bin/nexo` and the unit `/usr/lib/systemd/system/nexo.service`;
- creates the `nexo` system user and group and the directory `/var/lib/nexo/data` (default drive);
- installs `/etc/default/nexo` (owner `root:nexo`, mode 0640) with an **empty** root password: the
  service refuses to start until you set it;
- never enables, starts or restarts the service on its own; removing the package stops the service and
  keeps the data.

The `nexoctl` package installs the binary and the bash, zsh and fish completions. After the installation:

```bash
sudoedit /etc/default/nexo          # NEXO_ROOT_PASSWORD, NEXO_ENDPOINTS
sudo systemctl enable --now nexo
journalctl -u nexo -f
```

## Homebrew

```bash
brew install r3n3r0/tap/nexoctl
brew install r3n3r0/tap/nexo
```

The casks install the macOS and Linux binaries; the macOS binaries are signed with cosign but not notarized
by Apple (the cask removes the quarantine attribute).

## Docker

| Image | Contents |
|-------|----------|
| `ghcr.io/r3n3r0/nexo` | `/nexo` and `/nexoctl`, distroless base, non-root user (UID 65532), `VOLUME /data`, ports 9000 and 9001, `HEALTHCHECK` with `nexo healthcheck` |
| `ghcr.io/r3n3r0/nexoctl` | the CLI only, for CI pipelines and use from containers |

Tags: `vX.Y.Z`, plus `vX.Y` and `latest` for final releases. The images are multi-arch (linux/amd64,
linux/arm64), signed with cosign and carry SBOM and provenance attestations.

The same images are also published on Docker Hub as `docker.io/r3n3r0/nexo` and `docker.io/r3n3r0/nexoctl`
(same tags and digests, signatures and attestations included), for environments that only pull from Docker Hub or
mirror it. Pull from either registry; the examples below use ghcr.io:

```bash
docker pull ghcr.io/r3n3r0/nexo:latest     # GitHub Container Registry
docker pull r3n3r0/nexo:latest             # Docker Hub (docker.io/r3n3r0/nexo)
```

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersecret \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo:latest server /data
```

- **Secrets**: `NEXO_ROOT_USER_FILE` and `NEXO_ROOT_PASSWORD_FILE` read the credentials from files (Docker or
  Swarm secrets); the `_FILE` variable wins over the plain one. The same holds for
  `NEXO_KMS_SECRET_KEY_FILE` and for the `NEXO_SECRET_<NAME>_FILE` secrets.
- **Host volumes**: the container runs as UID 65532, so a mounted directory must belong to it:
  `sudo chown 65532:65532 /srv/nexo` before `-v /srv/nexo:/data`. Named volumes need nothing.
- **Multiple drives**: one volume per drive and ellipses in the arguments:
  `-v /mnt/d1:/data1 … -v /mnt/d4:/data4 ghcr.io/r3n3r0/nexo server /data{1...4}`.
- **TLS**: mount the certificates directory and set `NEXO_CERTS_DIR`; the healthcheck switches to
  HTTPS as well (see [TLS and certificates](security/tls.md)).
- **CLI from a container**:
  `docker run --rm -it --network host -v nexoctl-config:/home/nonroot/.nexoctl ghcr.io/r3n3r0/nexoctl ls local`.

To build the images from source: `make docker` and `make docker-nexoctl`.

## Docker Compose

The files in [`deploy/docker`](https://github.com/R3n3r0/Nexo/tree/main/deploy/docker) cover every
mode; you always start from `nexo.env`:

```bash
cd deploy/docker
cp nexo.env.example nexo.env              # set NEXO_ROOT_USER and NEXO_ROOT_PASSWORD
docker compose -f compose.single.yml up -d
```

| File | Deployment |
|------|------------|
| `compose.single.yml` | SNSD, one named volume (plus a `/cache` volume for the distributions cache) |
| `compose.snmd.yml` | SNMD with 4 volumes (`server /data{1...4}`) |
| `compose.cluster.yml` + `nginx.conf` | Cluster of 4 nodes × 2 drives behind an nginx on 9000 and 9001 |
| `compose.cluster-tls.yml` + `nginx-tls.conf` | The same cluster with TLS (`./certs` mounted on `/certs`; development certificates with `gen-dev-certs.sh`) |
| `compose.multi-proxy.yml` + `multi-proxy/` | Cluster behind two proxies (nginx and HAProxy) with different domains and origin secrets |
| `compose.observability.yml` | Prometheus and Grafana with the Nexo dashboard, to layer on top of the other files |

The published ports are changed with `NEXO_S3_PORT` and `NEXO_CONSOLE_PORT`, the image with `NEXO_IMAGE`
(in the shell or in a `.env` next to the files). The README of the directory describes each variant.

## Kubernetes (Helm)

The chart [`deploy/helm/nexo`](https://github.com/R3n3r0/Nexo/tree/main/deploy/helm/nexo) (Kubernetes
≥ 1.27) has two modes:

```bash
# standalone: one Deployment, one PVC per drive (1 drive = SNSD, more drives = SNMD)
helm install nexo oci://ghcr.io/r3n3r0/charts/nexo -n storage --create-namespace

# distributed: StatefulSet of 4 pods × 4 drives of 100 GiB, one pod per Kubernetes node
helm install nexo oci://ghcr.io/r3n3r0/charts/nexo -n storage --create-namespace \
  --set mode=distributed --set distributed.replicas=4 --set distributed.drivesPerNode=4 \
  --set persistence.size=100Gi --set podAntiAffinity=hard

helm test nexo -n storage        # cluster health and S3 round trip
```

- An empty root password is generated at install time and kept across upgrades (the chart's Secret,
  `auth.rootPassword` or `auth.existingSecret`).
- The chart generates the endpoints with ellipses, one argument per pool (`distributed.pools`), and rejects
  combinations that do not form valid erasure sets.
- TLS from a `kubernetes.io/tls` Secret (`tls.enabled`, `tls.secretName`), hot-reloaded on renewal;
  KMS from `kms.secretKey`/`kms.existingSecret` or KES (`kms.kes.*`).
- `serviceMonitor.enabled` creates a Prometheus Operator ServiceMonitor; three optional Ingresses (S3,
  console, distributions).
- An image upgrade proceeds one pod at a time; the PodDisruptionBudget (`maxUnavailable: 1`)
  protects the quorum while nodes are drained. Adding a pool, or changing the root credentials or
  TLS, requires restarting all the pods together.

All the values are documented in `values.yaml` and validated by `values.schema.json`.

## systemd

The unit [`deploy/systemd/nexo.service`](https://github.com/R3n3r0/Nexo/blob/main/deploy/systemd/nexo.service)
is the same for every mode and every node: it reads the environment from `/etc/default/nexo` and the endpoints
from `NEXO_ENDPOINTS`. It runs `nexo server --dry-run` before starting and runs as the `nexo` user in a
sandbox (`ProtectSystem=strict`, no capabilities; `systemd-analyze security` rates it 1.5 "OK").

Manual installation from an archive:

```bash
sudo install -m 0755 nexo nexoctl /usr/local/bin/
sudo useradd --system --home-dir /var/lib/nexo --shell /usr/sbin/nologin nexo
sudo install -d -m 0750 -g nexo /etc/nexo
sudo install -m 0640 -g nexo systemd/nexo.env.example /etc/default/nexo   # then edit it
sudo install -m 0644 systemd/nexo.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nexo
```

The file system is read-only for the service, except `/var/lib/nexo` and `/var/cache/nexo`. Drives in
other paths need a drop-in and the right owner:

```bash
sudo install -d -o nexo -g nexo -m 0750 /mnt/disk1 /mnt/disk2 /mnt/disk3 /mnt/disk4
sudo systemctl edit nexo
#   [Service]
#   ReadWritePaths=/mnt/disk1 /mnt/disk2 /mnt/disk3 /mnt/disk4
# in /etc/default/nexo:  NEXO_ENDPOINTS=/mnt/disk{1...4}
sudo systemctl restart nexo
```

- `systemctl reload nexo` sends `SIGHUP`: it reloads the TLS certificates without dropping connections.
- The unit is `Type=exec`: to wait until the node is ready use `nexo healthcheck --ready`.
- Ports below 1024 (`NEXO_WEB_ADDRESS=:80`) need `AmbientCapabilities=CAP_NET_BIND_SERVICE` and
  `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` in a drop-in.

## Docker Swarm

The stack [`deploy/swarm/stack.yml`](https://github.com/R3n3r0/Nexo/tree/main/deploy/swarm) starts a cluster
of 4 nodes × 2 drives with two nginx instances, the credentials as Docker secrets and upgrades one node at a time:

```bash
docker node update --label-add nexo.node=1 swarm-a      # … up to nexo.node=4
printf admin | docker secret create nexo_root_user -
openssl rand -base64 30 | tr -d '\n' | docker secret create nexo_root_password -
NEXO_TAG=v1.0.0 docker stack deploy -c stack.yml nexo
```

## From source

You need Go ≥ 1.27.2 (with `GOTOOLCHAIN=auto`, the default, an older toolchain downloads the one in `go.mod`
by itself), Node.js 22 with npm, and `make`:

```bash
git clone https://github.com/R3n3r0/Nexo.git && cd Nexo
make build            # React console + bin/nexo (console embedded) + bin/nexoctl
bin/nexo version
```

`make build-go` builds the two binaries without the console (useful for backend development: on port 9001
the Admin API works, and a "console not included" page takes the place of the console). `make help` lists
all the targets.

## Verifying the installation

```bash
nexo version
# checks configuration, topology, drive permissions, ports and certificates without starting anything
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersecret nexo server --dry-run /srv/nexo
# configuration OK: mode=snsd s3=:9000 console=:9001 endpoints=/srv/nexo
#   node: local (1 node(s))
#   pool 1: 1 erasure set(s) of 1 drive(s), parity 0
#   local drives: 1 writable, 0 formatted
```

`--dry-run` does not write to the drives and exits with a non-zero code (and the list of problems) if the
configuration is not valid.
