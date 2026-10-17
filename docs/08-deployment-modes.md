# 8. Deployment modes and release

Nexo has **two modes**, both first-class, with the **same binary** and the **same Docker image**:
the mode is inferred from the startup arguments, with no dedicated flags or different builds.

| Mode | Variants | Typical use |
|----------|----------|------------|
| **Single node** | **SNSD** (Single-Node Single-Drive): 1 directory, no erasure coding | Development, testing, edge, small volumes |
| | **SNMD** (Single-Node Multi-Drive): N drives on 1 node, erasure coding | A single server with several drives, drive fault tolerance |
| **Cluster** | **MNMD** (Multi-Node Multi-Drive): N nodes × M drives, one or more pools | Production, high availability, scalability |

All features (S3, IAM, policies, presigned URLs, versioning, lifecycle, events, SSE, dashboard, CLI) are available in
**all** modes. The only differences:

| Feature | SNSD | SNMD | Cluster |
|--------------|------|------|---------|
| S3 API, IAM, policies, presigned, POST policy | ✅ | ✅ | ✅ |
| Web dashboard and CLI | ✅ | ✅ | ✅ |
| Versioning, object lock, lifecycle, SSE, events, quotas | ✅ | ✅ | ✅ |
| Drive fault tolerance | ❌ (left to the storage subsystem) | ✅ up to P drives | ✅ up to P drives per set |
| Node fault tolerance | ❌ | ❌ | ✅ |
| Bitrot detection | ✅ (hash) / repair ❌ | ✅ with repair | ✅ with repair |
| Healing | — | ✅ | ✅ |
| Distributed locks, internode RPC | — (local locks) | — (local locks) | ✅ |
| Expansion | ❌ | ➜ migration | ✅ adding pools |
| "Cluster" dashboard page | Shows 1 node/1 drive | Shows 1 node and its drives | Pools, nodes, drives, sets |

## 8.1 Mode detection

```
nexo server /data                                   → SNSD
nexo server /data{1...4}                            → SNMD (EC on 4 drives)
nexo server http://node{1...4}:9000/data{1...4}     → Cluster (1 pool)
```

- Rules: all local paths and only 1 → SNSD; all local paths and > 1 → SNMD; at least one URL → Cluster
  (all endpoints must be URLs and the node must recognize its own through local IPs/hostnames).
- The mode is recorded in `format.json` (`"mode": "snsd" | "erasure"`): starting an SNSD drive with an erasure
  topology (or vice versa) fails with an explanatory error.
- The dashboard and `nexoctl admin info` always show the current mode.

### Implementation
- `ObjectLayer` has two implementations behind the same interface:
  - `fsObjects` (SNSD): direct mapping onto the file system, `xl.meta` for metadata/versions, no encoding;
  - `ServerPools → ErasureSets → ErasureObjects` (SNMD and Cluster).
- In SNMD the `StorageAPI` is always local and the locker is local: no RPC started, no network overhead.
- In Cluster the same components use `storageRESTClient` and `dsync`.
- **The whole S3/IAM/presigned/dashboard/CLI test suite runs in all three modes** (see §8.6).

## 8.2 Migration between modes
| From → To | Procedure |
|--------|-----------|
| SNSD → SNMD/Cluster | `nexoctl mirror --preserve old/ new/` + `nexoctl admin export/import` of IAM, config and bucket metadata (policies, lifecycle, events…) |
| SNMD → Cluster | As above, or adding a cluster pool and decommissioning the SNMD pool (P2) |
| Cluster → larger Cluster | Adding pools (no migration) |

Dedicated commands: `nexoctl admin cluster export ALIAS > backup.zip` (IAM + config + metadata of all buckets) and
`nexoctl admin cluster import ALIAS backup.zip` (`--dry-run`, `--mode merge|replace`). The import must run before
`mirror --preserve`: it creates the buckets with their versioning, so the target keeps all the copied versions (a
target bucket without versioning with several versions to copy is rejected). Verified by TM-04
(`test/integration/migration_test.go`).

## 8.3 Distribution as a binary

| Artifact | Contents |
|-----------|-----------|
| `nexo` | Server + embedded React dashboard (static, `CGO_ENABLED=0`) |
| `nexoctl` | CLI (static) |
| Platforms | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 (`nexoctl` also windows/arm64) |
| Packages | `.tar.gz`/`.zip`, `.deb`/`.rpm` (`nexo` with the systemd unit and `nexoctl` with shell completions), Homebrew cask, SHA256 checksums signed with cosign, SPDX SBOM (details in §8.7) |

Examples:

```bash
# Single node
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersecret ./nexo server /srv/nexo
# Console: http://localhost:9001   S3: http://localhost:9000

# Cluster (same command on every node)
./nexo server http://nexo{1...4}.lan:9000/mnt/disk{1...4}

# CLI
./nexoctl alias set local http://localhost:9000 admin supersecret
./nexoctl mb local/photos && ./nexoctl cp img.jpg local/photos/
./nexoctl share download --expire 2h local/photos/img.jpg
```

The binary includes `nexo version`, `nexo server --help` with examples for each mode, and a startup check
(`nexo server --dry-run …`) that validates topology, drive permissions, ports and certificates without starting the
service.

## 8.4 Distribution with Docker

### Images
| Image | Base | Contents |
|----------|------|-----------|
| `ghcr.io/r3n3r0/nexo` | `gcr.io/distroless/static:nonroot` | `nexo` + `nexoctl` (useful for `docker exec`) |
| `ghcr.io/r3n3r0/nexoctl` | `distroless/static` | CLI only, for CI pipelines and use from containers |

- Multi-arch (amd64/arm64), signed with cosign, SBOM attached, tags `latest`, `vX.Y.Z`, `vX.Y`.
- Non-root user (UID 65532), `VOLUME /data`, `EXPOSE 9000 9001`, `HEALTHCHECK` with `nexo healthcheck` (an internal
  subcommand, because the distroless image has no `curl`): it probes the console on loopback and, with
  `NEXO_CERTS_DIR` (or `--certs-dir`), switches to HTTPS trusting the directory's `public.crt` and `CAs/` and
  verifying the chain but not the names (the certificate covers the node names and the public ones, rarely
  `127.0.0.1`); with an explicit `--url` the TLS verification is complete.
- Secrets supported through `NEXO_ROOT_USER_FILE` / `NEXO_ROOT_PASSWORD_FILE` (Docker/Swarm secrets).

### Single node
```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersecret \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo server /data

# SNMD: several volumes
docker run -d -p 9000:9000 -p 9001:9001 \
  -v /mnt/d1:/data1 -v /mnt/d2:/data2 -v /mnt/d3:/data3 -v /mnt/d4:/data4 \
  --env-file nexo.env ghcr.io/r3n3r0/nexo server /data{1...4}

# CLI from the container
docker run --rm -it --network host ghcr.io/r3n3r0/nexoctl ls local
```

### Cluster with Docker Compose (`deploy/docker/compose.cluster.yml`)
```yaml
x-nexo: &nexo
  image: ghcr.io/r3n3r0/nexo:latest
  command: server --console-address :9001 http://nexo{1...4}:9000/data{1...2}
  env_file: nexo.env
  healthcheck:
    test: ["CMD", "/nexo", "healthcheck"]
    interval: 10s
services:
  nexo1: { <<: *nexo, hostname: nexo1, volumes: [d1-1:/data1, d1-2:/data2] }
  nexo2: { <<: *nexo, hostname: nexo2, volumes: [d2-1:/data1, d2-2:/data2] }
  nexo3: { <<: *nexo, hostname: nexo3, volumes: [d3-1:/data1, d3-2:/data2] }
  nexo4: { <<: *nexo, hostname: nexo4, volumes: [d4-1:/data1, d4-2:/data2] }
  lb:                       # nginx in front of the 4 nodes for S3 (9000) and console (9001)
    image: nginx:1.27-alpine
    ports: ["9000:9000", "9001:9001"]
    volumes: [./nginx.conf:/etc/nginx/nginx.conf:ro]
volumes: { d1-1: {}, d1-2: {}, d2-1: {}, d2-2: {}, d3-1: {}, d3-2: {}, d4-1: {}, d4-2: {} }
```

Files provided in `deploy/docker/`:
| File | Purpose |
|------|-------|
| `compose.single.yml` | SNSD with a named volume |
| `compose.snmd.yml` | SNMD with 4 volumes |
| `compose.cluster.yml` + `nginx.conf` | 4-node × 2-drive cluster with a load balancer (stickiness not required) |
| `compose.cluster-tls.yml` | Cluster with TLS (certificates mounted in `/certs`) |
| `compose.multi-proxy.yml` + `multi-proxy/` | 4-node × 2-drive cluster behind **two proxies** (nginx with `X-Forwarded-*`, HAProxy with PROXY protocol v2), different domains and origin secrets, a `/cache` volume per node (§8.5) |
| `compose.observability.yml` | Prometheus + Grafana with preconfigured Nexo dashboards |

### Other orchestrators
- **Docker Swarm** ([`deploy/swarm/stack.yml`](../deploy/swarm/README.md)): 4 nodes × 2 drives, each service bound to
  its Swarm node with `deploy.placement` (`node.labels.nexo.node`), so the named volumes follow it;
  `endpoint_mode: dnsrr` (task address, no IPVS VIP on the internode connections), credentials as Docker secrets
  (`NEXO_ROOT_*_FILE`), encrypted overlay network, two nginx with the configuration of `compose.cluster.yml`, updates
  one node at a time.
- **Kubernetes** ([`deploy/helm/nexo`](../deploy/helm/nexo/README.md)): `mode: standalone` (Deployment with one PVC per
  drive: SNSD or SNMD) or `mode: distributed` (`Parallel` StatefulSet with `replicas × pools` pods and
  `drivesPerNode` PVCs each, a headless service that publishes pods not yet ready, endpoints with ellipses generated
  by the template, PodDisruptionBudget, anti-affinity, probes on `/nexo/health/live|ready`, non-root user and
  read-only file system, Secrets for credentials and KMS, TLS from a `kubernetes.io/tls` Secret, ServiceMonitor,
  Ingress for S3/console/distributions, cache volume, `helm test` with cluster health and an S3 round trip).
- **systemd** ([`deploy/systemd`](../deploy/systemd/README.md)): sandboxed unit (`ProtectSystem=strict`, no
  capabilities, syscall filter: exposure 1.5 "OK" for `systemd-analyze security`), `/etc/default/nexo`,
  instructions for a cluster on several machines.

## 8.5 Reverse proxies and public URLs
Essential so that **presigned URLs** stay valid behind a proxy/load balancer:
- the SigV4 signature includes the `Host` header: the proxy must forward it unchanged, port included
  (`proxy_set_header Host $http_host`); the same header selects the distribution (docs/10 §10.2);
- `NEXO_SERVER_URL` (e.g. `https://s3.example.com`) is the public URL used by the console and the CLI to generate
  shareable links; `NEXO_BROWSER_REDIRECT_URL` is the public URL of the console;
- the real client (address and HTTPS) comes from trusted proxies (`NEXO_TRUSTED_PROXIES`) in **one** header family,
  chosen with `NEXO_FORWARDED_HEADERS`: `x-forwarded` (default: `X-Forwarded-For/Proto/Host`) or `forwarded`
  (RFC 7239); the other family is ignored, because a proxy forwards it as the client sent it. `X-Forwarded-For` is
  read from the right: entries added by the client on the left do not count;
- alternatively the **PROXY protocol** v1/v2 on the listeners listed in `NEXO_PROXY_PROTOCOL` (`s3`, `console`,
  `web`): the header is read before TLS, used only if the connection comes from a trusted proxy (read and ignored
  otherwise) and optional (a trusted proxy sending `X-Forwarded-*`, internode RPCs and health checks connect
  without it); the SSL TLV of `send-proxy-v2-ssl` makes the request HTTPS;
- unlimited body, no buffering, long timeouts; `/nexo/rpc/` never exposed on the public domain;
- a node that is stopping answers `503` (including `/nexo/health/ready`) until the health checks take it out of
  rotation: the proxy retries reads on another node (nginx `proxy_next_upstream ... http_503`, HAProxy
  `retry-on ... 503` limited to `GET/HEAD/OPTIONS`), the S3 SDKs retry writes.

Complete, commented configurations in `deploy/proxy/` (nginx, Caddy, Traefik, HAProxy, Cloudflare) for S3, console
and distributions, with the **origin secret** of each proxy (docs/10 §10.2).

### Several proxies in front of the same cluster (`compose.multi-proxy.yml`)
| Service | Proxy | Domains | Client towards Nexo |
|----------|-------|--------|-------------------|
| `proxy-a` | nginx (`multi-proxy/nginx.conf.template`) | `s3.alpha.example` (S3), `console.alpha.example`, `*.alpha.example` (distributions) | `X-Forwarded-*` |
| `proxy-b` | HAProxy (`multi-proxy/haproxy.cfg`) | `*.beta.example` (distributions) | PROXY protocol v2 with SSL TLV |

- The nodes trust only the two proxies (fixed addresses on the compose network, outside the `ip_range` assigned to
  the nodes) and accept the PROXY protocol on the S3 port (`NEXO_PROXY_PROTOCOL=s3`).
- Each proxy adds its own `X-Origin-Secret`; the distributions reference it by name (`${secret:proxy-a}`,
  `${secret:proxy-b}`), resolved on each node from `NEXO_SECRET_PROXY_A/B` (values in `.env`): a request that reaches
  a node without one of the two secrets is rejected (403).
- A `/cache` volume per node (distribution cache, docs/11).
- `multi-proxy/gen-certs.sh` creates the CA and development certificates of the two proxies, `multi-proxy/site.yaml`
  two example distributions (website and s3) served by both.

Verification: `make test-proxy` (`test/proxy`, build tag `proxy`, CI job `proxy`) runs TCD-01..03, TCC-C-05 and
TPS-19 on custom domains with two in-process Go proxies (one `X-Forwarded-*`, one PROXY protocol v2) in SNSD, SNMD
and cluster, and the **same** nginx/HAProxy **files** as `compose.multi-proxy.yml` with the real binaries in front of
a 4-node cluster (`TestRealProxies`; CI job with `NEXO_PROXY_REAL=1`). The `deploy-files` job also validates the
syntax of all the configurations in `deploy/proxy` and of the compose (`nginx -t`, `haproxy -c`, `caddy validate`,
`docker compose config`).

## 8.6 Mode-specific and release tests

| ID | Test | Expected |
|----|------|--------|
| TM-01 | S3 + IAM + presigned integration suite run as a `{SNSD, SNMD 4 drives, Cluster 4×2}` matrix | All green in the 3 modes |
| TM-02 | Playwright dashboard suite and CLI testscript in a `{SNSD, Cluster}` matrix | Green |
| TM-03 | Mode detection from arguments; topology incompatible with `format.json` | Correct mode / clear error |
| TM-04 | SNSD → Cluster migration with `mirror` + `admin cluster export/import` | Identical objects, versions, IAM, policies, lifecycle |
| TM-05 | Single-node `docker run`: healthcheck `healthy` within 10 s, console reachable, login OK | — |
| TM-06 | `docker compose -f compose.cluster.yml up`: cluster ready, 1 container stopped → no client errors | — |
| TM-07 | Container run as non-root with a host volume: correct permissions or a clear error | — |
| TM-08 | `*_FILE` secrets | Credentials read from files |
| TM-09 | Presigned URL generated behind nginx (compose) downloadable from outside | 200 |
| TM-10 | Release binaries: smoke startup on each OS/arch in CI (QEMU for arm64), `nexo version` consistent with the tag | — |
| TM-11 | Images: verifiable cosign signature, trivy scan without High/Critical vulnerabilities | — |
| TM-12 | `nexo server --dry-run` on valid/invalid configurations | Expected exit codes and messages |
| TM-13 | Upgrade from the previous release (`make test-upgrade`, `test/upgrade`): SNMD with data of every kind, rolling upgrade of a 4×2 cluster under load, downgrade and rollback of a node ([06 §6.1](06-test-plan.md)) | No data loss, no client errors |

**Upgrades and downgrades.** A release reads the drives written by the previous releases of the same major
(`format.json` v1, `xl.meta` 1.x, internode protocol v1): upgrading a single node is a restart with the new binary,
upgrading a cluster is one node at a time (SIGTERM, start, wait for `/nexo/health/cluster`) without interrupting the
clients behind the load balancer; the bootstrap only rejects nodes of another major. Downgrade (and rolling back a
node during a rolling upgrade) is supported between releases with the same formats, as today between M7 and the
current release: verified by TM-13. A release that changes a format bumps its version and the previous release
rejects the drives without touching them: in that case downgrade is not supported and must be announced in the
release notes.

Before upgrading the first node from `v0.7.0` (the "Upgrading" section of the [CHANGELOG](../CHANGELOG.md)):

1. with `--tls-internode-verify`, copy the CA that signs the node certificates into `CAs/` of the certificates
   directory on **all** nodes: the peers' client certificates are verified only with `CAs/` (and with the default
   leaf), no longer with the `public.crt` chain; without it, every upgraded node rejects the peers' RPC until quorum
   is lost. `nexo server --dry-run` flags this with a warning;
2. whoever serves distributions on the S3 port without `--server-url`/`--domain` sets `NEXO_S3_DISTRIBUTIONS=on` (or
   `NEXO_SERVER_URL`/`NEXO_DOMAIN`), otherwise the domains stop answering on port 9000;
3. during the rolling upgrade, GETs coordinated by an upgraded node with drives on `v0.7.0` nodes (without
   `storage/open-file`) hold the read lock until the end, as in `v0.7.0` (TestMixedClusterPinnedRead).

## 8.7 Release (T-9.5)

A `v*` tag starts [`.github/workflows/release.yml`](../.github/workflows/release.yml): GoReleaser v2.18.3 with
[`.goreleaser.yaml`](../.goreleaser.yaml), then the verification jobs. No long-lived secrets: the signatures are
**cosign keyless** (Sigstore, certificate issued for the workflow's OIDC identity).

### What a release produces

| Artifact | Details |
|-----------|-----------|
| Binaries | `nexo` (with the console, `-tags embedui`) for linux/darwin/windows × amd64/arm64 (not windows/arm64); `nexoctl` also for windows/arm64. Static (`CGO_ENABLED=0`), `-trimpath`, date and mtime = commit (reproducible); `nexo version` prints the tag |
| Archives | `nexo_X.Y.Z_<os>_<arch>.tar.gz` with LICENSE, NOTICE, COMMERCIAL-LICENSE.md, README, systemd unit and env file (`.zip` on Windows, with only LICENSE, NOTICE, COMMERCIAL-LICENSE.md and README: build and archive `nexo-windows`); `nexoctl_…` with the bash/zsh/fish completions; source archive `nexo_X.Y.Z_source.tar.gz` |
| Packages | `.deb` and `.rpm` for amd64/arm64: `nexo` (binary in `/usr/bin`, unit in `/usr/lib/systemd/system`, `/etc/default/nexo` as a `0640 root:nexo` conffile with an empty password, `nexo` user created by `preinstall` with `useradd` (deb: `Pre-Depends: passwd`) and declared in `/usr/lib/sysusers.d/nexo.conf`, `/var/lib/nexo/data`; never started or restarted by the scripts; removal stops the service and keeps the data) and `nexoctl` (binary and completions); `nexo` recommends `nexoctl` |
| Checksums | `checksums.txt` (SHA-256 of archives, packages, SBOMs, sources) + `checksums.txt.sigstore.json` (cosign bundle: signature, certificate, Rekor transparency log entry) |
| SBOM | One SPDX JSON document per archive (`<archive>.sbom.json`, syft v1.54.1): Go modules and toolchain of each binary. The packages contain the same linux binaries as the archives |
| Images | `ghcr.io/r3n3r0/nexo` (nexo + nexoctl) and `ghcr.io/r3n3r0/nexoctl`, linux/amd64+arm64 multi-arch index built by buildx from the already compiled binaries (`deploy/docker/Dockerfile*.goreleaser`, equivalent to the root Dockerfiles), BuildKit SBOM and provenance attestations (`mode=max`), cosign signature; tag `vX.Y.Z` and, for final releases, `vX.Y` and `latest`. Opt-in: the same index (same digest, attestations and signatures included) also as `docker.io/<namespace>/nexo` and `docker.io/<namespace>/nexoctl` when the repository variable `DOCKERHUB_NAMESPACE` is set (see [Docker Hub](#docker-hub-opt-in)) |
| Helm | `nexo-X.Y.Z.tgz` attached to the release and published in `oci://ghcr.io/r3n3r0/charts/nexo` (signed with cosign) |
| Homebrew | `nexo` and `nexoctl` casks in `r3n3r0/homebrew-tap` (`brew install r3n3r0/tap/nexoctl`), only if the `HOMEBREW_TAP_GITHUB_TOKEN` secret exists; the macOS binaries are not notarized: the cask removes the quarantine attribute |

### Verifying a release

```bash
TAG=v1.0.0
# Identity of the signatures: the release workflow of the tag, with the repository's
# capitalization on GitHub (cosign compares it exactly).
ID="https://github.com/R3n3r0/Nexo/.github/workflows/release.yml@refs/tags/$TAG"
ISSUER=https://token.actions.githubusercontent.com

# 1. signature of checksums.txt (cosign >= 2.4), then the downloaded files
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
sha256sum --ignore-missing -c checksums.txt          # macOS: shasum -a 256 --ignore-missing -c

# 2. SBOM of an archive (covered by the checksums)
jq -r '.packages[] | "\(.name) \(.versionInfo)"' nexo_${TAG#v}_linux_amd64.tar.gz.sbom.json | head

# 3. images: signature, SBOM and provenance attested in the multi-arch index
cosign verify ghcr.io/r3n3r0/nexo:$TAG --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
docker buildx imagetools inspect ghcr.io/r3n3r0/nexo:$TAG --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/r3n3r0/nexo:$TAG --format '{{ json .Provenance }}'

#    the Docker Hub copies (when published; namespace r3n3r0 here) are verified the same way, by their own name
cosign verify docker.io/r3n3r0/nexo:$TAG --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"

# 4. Helm chart
cosign verify ghcr.io/r3n3r0/charts/nexo:${TAG#v} --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
```

The release's `verify` job runs the same checks (TM-11) and the trivy scan of the images (High/Critical with a fix
available: the job fails); the `smoke` job (TM-10) extracts the archives on linux/amd64, linux/arm64 (QEMU),
darwin/arm64, darwin/amd64 (Rosetta) and windows/amd64 and runs [`packaging/smoke.sh`](../packaging/smoke.sh):
version equal to the tag, `--dry-run`, server on one drive, console served, S3 round trip with `nexoctl`, shutdown
with SIGTERM (on Windows only version and `--dry-run`).

### Docker Hub (opt-in)

ghcr.io is always published; Docker Hub is added by repository settings only, without touching the files:

| GitHub setting | Kind | Value |
|----------------|------|-------|
| `DOCKERHUB_NAMESPACE` | variable (Settings → Secrets and variables → Actions → Variables) | Docker Hub user or organization that owns the `nexo` and `nexoctl` repositories (lowercased by the workflow). Empty or missing: the release publishes to ghcr.io only, exactly as before |
| `DOCKERHUB_USERNAME` | secret | Docker Hub user that owns the access token (an admin of the repositories for an organization namespace) |
| `DOCKERHUB_TOKEN` | secret | Docker Hub personal access token with the **Read, Write, Delete** scope (Delete is required by the description update), never the password |

With the variable set, `release.yml`:

1. checks that both secrets exist (the job fails with a clear error otherwise) and logs in to `docker.io` with
   `docker/login-action`;
2. passes `DOCKERHUB_NAMESPACE` to GoReleaser: the templated image name `docker.io/<namespace>/nexo` (and
   `nexoctl`) of `dockers_v2` renders, and buildx pushes the same multi-arch index with its SBOM and provenance
   attestations to both registries in one build (when the variable is empty the template renders `""` and
   GoReleaser drops the name). `docker_signs` signs every
   `name:tag` by digest, so the cosign signatures are stored in both registries; the release notes list the
   Docker Hub `docker pull` commands too;
3. `verify` checks signature and SBOM of the Docker Hub images as well, and that both registries serve the same
   digest;
4. `dockerhub-description` pushes [`deploy/docker/DOCKERHUB.md`](../deploy/docker/DOCKERHUB.md) (image names
   rewritten to the namespace) and a short description to both repositories with
   `peter-evans/dockerhub-description@v5`.

One-time setup on Docker Hub: create the repositories `nexo` and `nexoctl` in the namespace as **public** before
the first release (a repository created by the first push takes the default visibility of the account, which can be
private, and an organization may not let its members create repositories), create the access token (Account
settings → Personal access tokens), then the variable and the two secrets above. The ghcr.io packages stay the
reference: the Helm chart is published only to `oci://ghcr.io/r3n3r0/charts`.

### Reproducing and checking locally (without publishing)

| Command | What it does |
|---------|---------|
| `make release-tools` | Installs GoReleaser, Helm, kubeconform and syft in `bin/tools` at the versions pinned in the Makefile |
| `make release-check` | `goreleaser check` |
| `make release-snapshot` | The whole release in `dist/` (version `X.Y.Z-next`), without images or signatures (`RELEASE_SKIP=docker,sign`; with a Docker daemon `RELEASE_SKIP=sign` also builds the images) |
| `make release-smoke` | `packaging/smoke.sh` on the snapshot's linux/amd64 binaries and, with `qemu-aarch64-static`, linux/arm64 |
| `make helm-lint` | `helm lint --strict`, `helm template` and `kubeconform -strict` for each Kubernetes version (1.27 and 1.34, explicit `--kube-version`: a helm built with `go install` assumes 1.20), ServiceMonitor CRD, with the default values and `deploy/helm/nexo/ci/*-values.yaml`; then the endpoints generated for each mode (SNSD, SNMD, distributed, two pools, TLS) and the errors of invalid values |
| `make systemd-verify` | `systemd-analyze verify` and `security` (exposure ≤ 2.0) of the unit |
| `make hadolint` / `make packaging` | Dockerfiles; all the checks that do not require Docker |

The CI job `packaging` runs on every pull request the snapshot with the images, TM-10 on amd64 and arm64, the
installation of the `.deb` packages with the runner's systemd (a service that refuses to start without a password,
then starts, answers, S3 round trip, `reload`, `stop`, removal that keeps the data), the `.rpm` packages in a Fedora
container, the chart, the unit and the Dockerfiles; the `deploy-files` job validates the Swarm stack
(`docker stack config`).

### Validated locally and only configured

Last verification (GoReleaser v2.18.3, official Helm v3.22.0 and `go install`, kubeconform v0.7.0, syft v1.54.1,
hadolint v2.14.0, actionlint v1.7.7 with shellcheck 0.9.0, systemd 255, QEMU user, Docker CLI without a daemon):

| Item | Outcome |
|----------|-------|
| `goreleaser check` | Valid, no deprecations |
| `goreleaser release --snapshot --clean --skip=publish,sign,docker` | 11 binaries (`nexo` with the console, `nexoctl`), 11 archives + sources, 8 deb/rpm packages, 11 SPDX 2.3 SBOMs, `checksums.txt` (31 entries, all verified with `sha256sum -c`), Homebrew casks `nexo` and `nexoctl` |
| Binaries | `nexo version` (linux/amd64) = snapshot version and commit; `file`: static ELF x86-64/aarch64, Mach-O x86_64/arm64, PE32+ x86-64/Aarch64 |
| TM-10 (`make release-smoke`) | Green on linux/amd64 and linux/arm64 (`qemu-aarch64-static`): version, `--dry-run`, server, console served, S3 round trip, SIGTERM |
| Packages | `dpkg-deb -I/-c`, `rpm -qip/-qlvp/--scripts`: files, permissions and owners (`root:nexo 0640` for `/etc/default/nexo`, `nexo:nexo 0750` for `/var/lib/nexo/data`), conffiles, scripts and `Recommends` |
| Helm (`make helm-lint`) | `lint --strict`, `template` and `kubeconform -strict` green for default, standalone, SNMD, distributed and distributed TLS on Kubernetes 1.27 and 1.34; assertions on the endpoints and on invalid values |
| Compose and Swarm | `docker compose config` of all `compose.*.yml`, `docker stack config -c deploy/swarm/stack.yml` |
| systemd | `systemd-analyze verify` clean, `security`: exposure 1.5 "OK" |
| Dockerfile | hadolint with no findings on the four Dockerfiles |
| Workflows and scripts | actionlint on `release.yml` and `ci.yml` (with shellcheck: two intentional SC2046 remain in the `lint-go` and `test-go` jobs), shellcheck of the scripts in `packaging/` and `deploy/helm/check.sh` |
| TLS healthcheck | Real server with `NEXO_CERTS_DIR`: the previous HTTP probe got `400`, the new one is `ok` even with a certificate that names only `nexo1` (hot reloaded) and fails with a foreign CA |

Only configured (requires a Docker daemon, the workflow's OIDC identity or publishing): `dockers_v2` multi-arch
images with SBOM/provenance attestations, cosign keyless signatures of checksums, images and chart, push to ghcr.io
(and to Docker Hub when configured) and to the Homebrew tap, the release's `verify` (TM-11) and `smoke` jobs on darwin/windows, package installation with
systemd and in Fedora (CI job `packaging`).

License: AGPL-3.0-only with the attribution terms of `NOTICE` (AGPL §7(b)) and commercial licenses
(`COMMERCIAL-LICENSE.md` at the repository root); `LICENSE` is in archives, packages, images and charts, `NOTICE` in
archives and packages. Open release decisions: the `r3n3r0/homebrew-tap` repository with the
`HOMEBREW_TAP_GITHUB_TOKEN` secret, public visibility of the ghcr.io packages (private on first publication), the
Docker Hub repositories with the `DOCKERHUB_NAMESPACE` variable and the `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN`
secrets (optional). The
toolchain is Go 1.27.2 (docs/security/threat-model.md §10.1): govulncheck finds no reachable vulnerabilities, so trivy
in the `verify` job has no known standard library vulnerabilities left to report.
