# Nexo Helm chart

Nexo, the S3-compatible object storage server with an embedded web console, on Kubernetes ≥ 1.27
(docs/08 §8.4). Two modes, the same image:

| `mode` | Resources | Nexo mode |
|--------|-----------|-----------|
| `standalone` (default) | Deployment (1 replica, `Recreate`) + one PVC per drive | `standalone.drives=1`: SNSD (`/data`); `>1`: SNMD with erasure coding (`/data1 … /dataN`) |
| `distributed` | StatefulSet (`replicas × pools` pods, `Parallel`), headless Service, PodDisruptionBudget, M PVCs per pod | Cluster: erasure sets across the pods |

The chart is attached to every [GitHub release](https://github.com/R3n3r0/Nexo/releases) as `nexo-X.Y.Z.tgz`
(with its cosign bundle `nexo-X.Y.Z.tgz.sigstore.json`); it is not published to an OCI registry. The image is
`docker.io/paolor/nexo`.

```bash
TAG=v1.0.0
CHART="https://github.com/R3n3r0/Nexo/releases/download/$TAG/nexo-${TAG#v}.tgz"

# standalone, one 10 GiB drive, generated root password
helm install nexo "$CHART" -n storage --create-namespace

# cluster of 4 pods x 4 drives of 100 GiB, one pod per Kubernetes node
helm install nexo "$CHART" -n storage --create-namespace \
  --set mode=distributed --set distributed.replicas=4 --set distributed.drivesPerNode=4 \
  --set persistence.size=100Gi --set podAntiAffinity=hard

helm test nexo -n storage      # cluster health (write quorum of every erasure set) + S3 round trip with nexoctl
```

From a checkout: `helm install nexo deploy/helm/nexo …`. Every value is documented in
[values.yaml](values.yaml) and checked by [values.schema.json](values.schema.json); invalid
combinations (an erasure set that cannot be formed, TLS without a Secret, …) fail the rendering
with an explicit message.

## Distributed endpoints

The chart generates the endpoints of `nexo server` with the ellipsis syntax, one argument per pool:

```
http://<release>-nexo-{0...3}.<release>-nexo-hl.<namespace>.svc.cluster.local:9000/data{1...4}
http://<release>-nexo-{4...7}.<release>-nexo-hl.<namespace>.svc.cluster.local:9000/data{1...4}   # pools: 2
```

- The headless Service publishes the pods **before** they are ready (`publishNotReadyAddresses`):
  the nodes find each other during the bootstrap; `podManagementPolicy: Parallel` starts them together.
- Every node recognizes its own endpoints by its FQDN (in `/etc/hosts` of a StatefulSet pod).
- The total drives of a pool (`replicas × drivesPerNode`) must split into erasure sets of 2–16 drives
  spread evenly on the pods (the chart checks it).
- Expanding with a pool (`distributed.pools`), changing the root credentials or the TLS settings
  changes every node: restart all the pods together. A normal upgrade (new image) rolls one pod at a
  time; the PodDisruptionBudget (`maxUnavailable: 1`) keeps the quorum during node drains.
- The PVCs of a StatefulSet survive `helm uninstall` (Kubernetes default), so do the PVCs of the
  standalone mode and the chart Secret (`helm.sh/resource-policy: keep`): reinstalling with the same
  release name finds the same data and credentials.

## Credentials, KMS, TLS

| Value | Effect |
|-------|--------|
| `auth.rootUser`, `auth.rootPassword` | Chart Secret (`root-user`, `root-password`); an empty password is generated at install and kept on upgrades |
| `auth.existingSecret` | Your Secret instead (keys `existingSecretUserKey`, `existingSecretPasswordKey`) |
| `kms.secretKey` / `kms.existingSecret` | Builtin KMS master key (`NEXO_KMS_SECRET_KEY_FILE`): SSE-S3 and SSE-KMS |
| `kms.kes.*` | External KES (`NEXO_KMS_ENDPOINT`, mutual TLS from `kms.kes.existingSecret`: `client.crt`, `client.key`, `ca.crt`) |
| `tls.enabled`, `tls.secretName` | `kubernetes.io/tls` Secret mounted as `public.crt`/`private.key` in `NEXO_CERTS_DIR`, hot reloaded on renewal; https endpoints and probes |
| `tls.caSecretName` | CAs trusted by the nodes (`CAs/`), used by the ServiceMonitor and the Helm tests too |

The secrets reach the server as files (`*_FILE` variables on a projected volume, mode 0440), never
as plain environment variables. With TLS in distributed mode the certificate must cover
`*.<release>-nexo-hl.<namespace>.svc.cluster.local` (internode RPC), `<release>-nexo.<namespace>.svc`
and `<release>-nexo.<namespace>.svc.cluster.local` (Service, ServiceMonitor, Helm tests) and the public
names; with `tls.internodeVerify` it must also be valid as a client certificate. A cert-manager
`Certificate` with these `dnsNames` and `secretName: <tls.secretName>` fits.

## Ingress

Three optional Ingresses on the same Service: `ingress.s3` (S3 API), `ingress.console` (console and
Admin API) and `ingress.distributions` (custom domains of the distributions, docs/10: the S3 port
chooses the distribution by the `Host` header). Requirements of the controller (docs/08 §8.5):

- keep the `Host` header (presigned URLs are signed with it), no body size limit, no request
  buffering, long timeouts — for ingress-nginx:
  `nginx.ingress.kubernetes.io/proxy-body-size: "0"`, `…/proxy-request-buffering: "off"`,
  `…/proxy-read-timeout: "3600"`;
- do not publish `/nexo/rpc/` (internode RPC) on the S3 host: block it at the controller (e.g. a
  Traefik middleware, an nginx `location` in the controller configuration, a Gateway API
  `HTTPRoute` that routes only the other paths);
- list the controller pods in `config.trustedProxies` so that the client address and scheme come
  from `X-Forwarded-*`.

`config.serverURL` and `config.browserRedirectURL` default to the first host of `ingress.s3` and
`ingress.console` (https when the Ingress has `tls`).

## Monitoring

`serviceMonitor.enabled` creates a Prometheus Operator ServiceMonitor: `/nexo/metrics/v1/node` from
every pod and, once (through the Service, from the target of the first pod), `/nexo/metrics/v1/cluster`
(and `/bucket` with `serviceMonitor.bucket`). With the default `NEXO_PROMETHEUS_AUTH_TYPE=jwt`, store
a token of `nexoctl admin prometheus generate` in a Secret and set `serviceMonitor.bearerTokenSecret`;
or set `config.prometheusAuthType=public`. Dashboards and alerts: `deploy/docker/observability/`.

## Media worker

`mediaWorker.enabled=true` adds [nexo-media-worker](https://github.com/R3n3r0/Nexo/blob/main/docs/user/media-worker.md) (renditions,
posters and thumbnails of the videos, variants of the images) and wires it to Nexo:

```bash
helm upgrade --install nexo "$CHART" -n storage --set mediaWorker.enabled=true
nexoctl mb --with-hls --with-images nexo/media      # videos and images processed by the worker
```

| Resource | Content |
|----------|---------|
| Secret `<release>-nexo-media-worker` | `access-key`, `secret-key`, `auth-token`: generated at install, kept on upgrades and by `helm uninstall` (or `mediaWorker.auth.existingSecret` with the same keys) |
| Deployment, 1 pod (`Recreate`) | image `docker.io/paolor/nexo-media-worker`, non-root, read-only root file system (`/data` and `/tmp` writable), probes on `/healthz` (port 9600) |
| PVC (`mediaWorker.persistence`, 20 GiB) | `/data`: the job queue (it must persist), the work files and OpenH264 |
| Service `<release>-nexo-media-worker:9600` | jobs endpoint, the `endpoint` of the target |
| Job `<release>-nexo-media-worker-setup` (hook `post-install,post-upgrade`) | the least-privilege policy (`mediaWorker.setup.buckets`, every bucket when empty), the user of the worker, the target `notify_media_worker` (`mediaWorker.setup.target`) with the endpoint and the token |

The target is part of the dynamic configuration of Nexo, which has no environment variable: the setup Job
writes it through the Admin API with the root credentials, using `nexoctl` from the server image and the
script [files/media-worker-setup.sh](files/media-worker-setup.sh) in `mediaWorker.setup.shellImage`
(busybox; the images of Nexo have no shell). The script is idempotent: it waits for Nexo
(`mediaWorker.setup.timeout`, 600 s: give `helm install` a longer `--timeout` for a large cluster), updates
the policy, creates the user or re-creates it when the secret key of the Secret changed, and sets the
target. With `mediaWorker.setup.enabled=false` do the same by hand ([docs](https://github.com/R3n3r0/Nexo/blob/main/docs/user/media-worker.md#setup),
the NOTES print the command).

- **One replica.** Every worker keeps its own queue on its own volume, without coordination: behind one
  Service the jobs of the same object would race on different pods, and a job delivered again would be
  processed twice. Scale it up: `mediaWorker.resources`, `mediaWorker.concurrency`.
- **Memory.** A video job needs about 1 GiB, an image job about 2 GiB (AVIF): the limit (3 GiB by
  default) must cover `concurrency` jobs; `mediaWorker.imageMemory` bounds the image encoders.
- **OpenH264.** Downloaded from `ciscobinary.openh264.org` at the first start into the volume (egress
  needed once; the startup probe allows 5 minutes). Without egress: `mediaWorker.openh264.path` and
  `extraVolumes`/`extraVolumeMounts` with a library fetched by `nexo-media-worker fetch-openh264`, or a
  mirror in `mediaWorker.openh264.downloadURL`.
- **TLS.** With `tls.enabled` the worker and the Job use `https://` and trust `tls.caSecretName`.
- **NetworkPolicy.** With `networkPolicy.enabled` only the Nexo pods reach the jobs endpoint, and the
  worker and its Job reach Nexo even when `networkPolicy.allowedFrom` restricts S3 and console.
- `helm test` also probes the jobs endpoint through its Service.

## Security defaults

Non-root (UID/GID 65532, `fsGroup` for the volumes), read-only root file system (`/tmp` is an
emptyDir), no capability, no privilege escalation, `RuntimeDefault` seccomp profile, no service
account token. Optional `networkPolicy`: the pods reach each other on every port, S3 and console
only from `networkPolicy.allowedFrom`.

## Checks

`make helm-lint` runs `helm lint --strict`, `helm template` and `kubeconform -strict` (Kubernetes
schemas and the ServiceMonitor CRD) on the defaults and on every `ci/*-values.yaml`
(standalone, SNMD, distributed, distributed with TLS and two pools, the media worker in both modes); the
CI job `packaging` runs it on every pull request.
