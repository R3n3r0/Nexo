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

## Security defaults

Non-root (UID/GID 65532, `fsGroup` for the volumes), read-only root file system (`/tmp` is an
emptyDir), no capability, no privilege escalation, `RuntimeDefault` seccomp profile, no service
account token. Optional `networkPolicy`: the pods reach each other on every port, S3 and console
only from `networkPolicy.allowedFrom`.

## Checks

`make helm-lint` runs `helm lint --strict`, `helm template` and `kubeconform -strict` (Kubernetes
schemas and the ServiceMonitor CRD) on the defaults and on every `ci/*-values.yaml`
(standalone, SNMD, distributed, distributed with TLS and two pools); the CI job `packaging`
runs it on every pull request.
