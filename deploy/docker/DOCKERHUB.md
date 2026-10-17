<!--
README of the Docker Hub repositories nexo and nexoctl, pushed by the
dockerhub-description job of .github/workflows/release.yml (at most 25,000
bytes). The job replaces "docker.io/r3n3r0/" with the configured
DOCKERHUB_NAMESPACE; links must be absolute.
-->

# Nexo

S3-compatible object storage server with an embedded web console: a single static binary for a single drive, a
single node with several drives (erasure coding) or a distributed cluster.

- **Source and issues**: <https://github.com/R3n3r0/Nexo>
- **Documentation**: <https://github.com/R3n3r0/Nexo/tree/main/docs/user>
- **Also on**: `ghcr.io/r3n3r0/nexo` and `ghcr.io/r3n3r0/nexoctl` (the same images, same digests)

## Images

| Image | Contents |
|-------|----------|
| `nexo` | the server `/nexo` and the CLI `/nexoctl`, distroless base, non-root user (UID 65532), `HEALTHCHECK` with `nexo healthcheck` |
| `nexoctl` | the CLI only, for CI pipelines and use from containers |

Both are multi-arch (linux/amd64, linux/arm64), signed with cosign keyless and carry SBOM and provenance
attestations.

## Quick start

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersecret \
  -v nexo-data:/data docker.io/r3n3r0/nexo:latest server /data
```

Then open the console at <http://localhost:9001> or point any S3 client at <http://localhost:9000> with the root
credentials. With the CLI:

```bash
docker run --rm -it --network host -v nexoctl-config:/home/nonroot/.nexoctl docker.io/r3n3r0/nexoctl \
  alias set local http://localhost:9000 admin supersecret
docker run --rm -it --network host -v nexoctl-config:/home/nonroot/.nexoctl docker.io/r3n3r0/nexoctl ls local
```

The deployment mode follows the arguments of `nexo server`: several drives with
`server /data{1...4}` (one volume per drive), a cluster with `server http://node{1...4}/data{1...4}`.

## Tags

| Tag | Meaning |
|-----|---------|
| `vX.Y.Z` | one release (also pre-releases such as `v1.1.0-rc.1`) |
| `vX.Y` | the latest final release of that minor version |
| `latest` | the latest final release |

## Ports and volumes

| | |
|---|---|
| `9000` | S3 API (and internode RPC of a cluster) |
| `9001` | web console and Admin API |
| `/data` | default data volume (`VOLUME /data`); a host directory must belong to UID 65532 (`chown 65532:65532`) |

## Environment variables

| Variable | Description |
|----------|-------------|
| `NEXO_ROOT_USER`, `NEXO_ROOT_PASSWORD` | root credentials, required (user ≥ 3 characters, password ≥ 8) |
| `NEXO_ROOT_USER_FILE`, `NEXO_ROOT_PASSWORD_FILE` | the same, read from files (Docker or Swarm secrets); they win over the plain variables |
| `NEXO_ADDRESS`, `NEXO_CONSOLE_ADDRESS` | listen addresses, default `:9000` and `:9001` |
| `NEXO_SERVER_URL`, `NEXO_BROWSER_REDIRECT_URL` | public URLs behind a reverse proxy |
| `NEXO_TRUSTED_PROXIES` | proxies whose `X-Forwarded-*` headers are trusted |
| `NEXO_DOMAIN` | enables virtual-host bucket addressing (`<bucket>.<domain>`) |
| `NEXO_REGION` | default region (`us-east-1`) |
| `NEXO_CERTS_DIR` | TLS: directory with `public.crt` and `private.key` (the healthcheck follows) |
| `NEXO_KMS_SECRET_KEY` / `_FILE` | master key of the built-in KMS for server-side encryption |
| `NEXO_LOG_LEVEL`, `NEXO_LOG_FORMAT` | `debug`/`info`/`warn`/`error`, `json`/`text` |

The complete list is in the
[configuration reference](https://github.com/R3n3r0/Nexo/blob/main/docs/user/configuration.md).

## Documentation

- [Installation](https://github.com/R3n3r0/Nexo/blob/main/docs/user/installation.md) (Docker, Compose, Swarm, Helm,
  packages)
- [Quick start](https://github.com/R3n3r0/Nexo/blob/main/docs/user/quickstart.md)
- [Deployment modes](https://github.com/R3n3r0/Nexo/blob/main/docs/user/deployment-modes.md)
- [Compose files](https://github.com/R3n3r0/Nexo/tree/main/deploy/docker) and
  [Helm chart](https://github.com/R3n3r0/Nexo/tree/main/deploy/helm/nexo)
  (`oci://ghcr.io/r3n3r0/charts/nexo`)
- [Verifying a release](https://github.com/R3n3r0/Nexo/blob/main/docs/08-deployment-modes.md) (cosign signatures,
  SBOM, provenance)

## License

Nexo is released under the [GNU AGPL-3.0](https://github.com/R3n3r0/Nexo/blob/main/LICENSE) with the attribution
terms of [NOTICE](https://github.com/R3n3r0/Nexo/blob/main/NOTICE); commercial licenses are available, see
[COMMERCIAL-LICENSE.md](https://github.com/R3n3r0/Nexo/blob/main/COMMERCIAL-LICENSE.md). The images contain
`/LICENSE`, `/NOTICE` and `/COMMERCIAL-LICENSE.md`.
