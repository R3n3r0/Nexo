<!--
README of the Docker Hub repository nexo-operator, pushed by the
dockerhub-description job of .github/workflows/release.yml (at most 25,000
bytes). The job replaces "docker.io/paolor/" with the configured
DOCKERHUB_NAMESPACE (default paolor); links must be absolute.
-->

# Nexo operator

`nexo-operator` runs [Nexo](https://github.com/R3n3r0/Nexo) clusters on Kubernetes from one resource,
`NexoCluster`: a StatefulSet per pool, the S3 and console Services and a disruption budget. It replaces the
pods itself when their template changes (a new image, new settings), one at a time and only while the
cluster can lose a node without losing its write quorum; it never deletes the volumes of the drives.

- **Documentation**: <https://github.com/R3n3r0/Nexo/blob/main/docs/user/kubernetes-operator.md>
- **Source and issues**: <https://github.com/R3n3r0/Nexo>

The image is multi-arch (linux/amd64, linux/arm64), distroless, non-root (UID 65532), signed with cosign
keyless and carries SBOM and provenance attestations.

## Quick start

```bash
# the operator (CRD, RBAC, Deployment with this image), from the release of the same tag
kubectl apply --server-side -f https://github.com/R3n3r0/Nexo/releases/latest/download/nexo-operator.yaml

# a cluster of 4 servers x 4 drives
kubectl create namespace storage
kubectl -n storage create secret generic lake-credentials \
  --from-literal=root-user=lake-admin --from-literal=root-password='a-long-random-secret'
kubectl apply -f - <<'YAML'
apiVersion: nexo.r3n3r0.github.io/v1alpha1
kind: NexoCluster
metadata: {name: lake, namespace: storage}
spec:
  image: docker.io/paolor/nexo:latest
  credentials: {secretName: lake-credentials}
  pools:
    - {name: pool-0, servers: 4, volumesPerServer: 4, storage: 100Gi}
YAML
kubectl -n storage get nexo
```

## Verify

```bash
cosign verify docker.io/paolor/nexo-operator:<tag> \
  --certificate-identity 'https://github.com/R3n3r0/Nexo/.github/workflows/release.yml@refs/tags/<tag>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

License: AGPL-3.0-only.
