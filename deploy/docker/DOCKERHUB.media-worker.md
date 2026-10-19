<!--
README of the Docker Hub repository nexo-media-worker, pushed by the
dockerhub-description job of .github/workflows/release.yml (at most 25,000
bytes). The job replaces "docker.io/paolor/" with the configured
DOCKERHUB_NAMESPACE (default paolor); links must be absolute.
-->

# Nexo media worker

`nexo-media-worker` derives the renditions (H.264/AAC), posters and timeline thumbnails of the videos, and
the variants of the images (AVIF, WebP, JPEG, PNG at several widths), that the media rules of the buckets of
[Nexo](https://github.com/R3n3r0/Nexo) select. Nexo sends it jobs through its `notify_media_worker` target;
the worker reads the sources and writes its outputs through the S3 API, with credentials of its own.

- **Documentation**: <https://github.com/R3n3r0/Nexo/blob/main/docs/user/media-worker.md>
- **Source and issues**: <https://github.com/R3n3r0/Nexo>

The image is multi-arch (linux/amd64, linux/arm64), distroless (glibc), non-root (UID 65532), signed with
cosign keyless and carries SBOM and provenance attestations. It contains no ffmpeg and **no H.264
codec**: at its first start the worker downloads the Cisco build of OpenH264 into `/data/lib` (checksum
pinned in the worker), so that the Cisco patent license applies.

> OpenH264 Video Codec provided by Cisco Systems, Inc.

## Quick start

```bash
# 1. on Nexo: a least-privilege user for the worker (policy in the documentation)
nexoctl admin policy create local media-worker media-worker-policy.json
nexoctl admin user add local media-worker 'a-long-random-secret' --policy media-worker

# 2. the worker
TOKEN="$(openssl rand -hex 32)"
docker run -d --name nexo-media-worker -v media-worker:/data \
  -e NEXO_WORKER_S3_ENDPOINT=http://nexo:9000 \
  -e NEXO_WORKER_ACCESS_KEY=media-worker -e NEXO_WORKER_SECRET_KEY='a-long-random-secret' \
  -e NEXO_WORKER_AUTH_TOKEN="$TOKEN" docker.io/paolor/nexo-media-worker

# 3. on Nexo: send the jobs to the worker
nexoctl admin config set local notify_media_worker enable=on endpoint=http://nexo-media-worker:9600/jobs auth_token="$TOKEN"
nexoctl mb --with-hls --with-images local/media
```

Docker Compose (overlay that also creates the user and the target), Swarm, Helm (`mediaWorker.enabled=true`)
and systemd: see the [setup](https://github.com/R3n3r0/Nexo/blob/main/docs/user/media-worker.md#setup).

## Tags

| Tag | Meaning |
|-----|---------|
| `vX.Y.Z` | one release (also pre-releases such as `v1.1.0-rc.1`) |
| `vX.Y` | the latest final release of that minor version |
| `latest` | the latest final release |

Run the worker of the same version as Nexo.

## Port and volume

| | |
|---|---|
| `9600` | jobs endpoint (`POST /jobs`, bearer token) and `GET /healthz`; reachable by Nexo only |
| `/data` | job queue (must persist: an acknowledged job lives only there), work files, downloaded OpenH264; a host directory must belong to UID 65532 |

## Environment variables

| Variable | Description |
|----------|-------------|
| `NEXO_WORKER_S3_ENDPOINT` | URL of the S3 API of Nexo (required) |
| `NEXO_WORKER_ACCESS_KEY`, `NEXO_WORKER_SECRET_KEY` | credentials of the worker (required) |
| `NEXO_WORKER_AUTH_TOKEN` | bearer token, the `auth_token` of the `notify_media_worker` target (required) |
| `NEXO_WORKER_CONCURRENCY` | jobs processed at once (default 1; each job uses every core) |
| `NEXO_WORKER_IMAGE_MEMORY` | memory budget of the image encoders, bytes (default 2 GiB) |
| `NEXO_WORKER_OPENH264` | path of a libopenh264 2.4.1 instead of the download |
| `NEXO_WORKER_LOG_LEVEL`, `NEXO_WORKER_LOG_FORMAT` | `debug`/`info`/`warn`/`error`, `text`/`json` |

Memory: about 1 GiB per video job and 2 GiB per image job. Run **one** worker per Nexo deployment and scale
it up (cores, `NEXO_WORKER_CONCURRENCY`): each worker keeps its own queue.

## License

Nexo is released under the [GNU AGPL-3.0](https://github.com/R3n3r0/Nexo/blob/main/LICENSE) with the attribution
terms of [NOTICE](https://github.com/R3n3r0/Nexo/blob/main/NOTICE); commercial licenses are available, see
[COMMERCIAL-LICENSE.md](https://github.com/R3n3r0/Nexo/blob/main/COMMERCIAL-LICENSE.md). The image contains
`/LICENSE`, `/NOTICE` and `/COMMERCIAL-LICENSE.md`. OpenH264 is not part of the image: it is downloaded from
Cisco under the [Cisco binary license](https://www.openh264.org/BINARY_LICENSE.txt).
