# Kubernetes operator

The Nexo operator runs Nexo clusters on Kubernetes from one resource, `NexoCluster`: it creates a
StatefulSet per pool, the Services and a disruption budget, and replaces the pods itself when their
template changes, one at a time and only while the cluster can lose a node. The Helm chart
([Installation](installation.md)) remains the simplest way to run a single deployment; the operator adds
health-gated updates, pool expansion and the protection of the drives.

## Installation

```bash
kubectl apply -k deploy/operator          # namespace nexo-operator, CRD, ClusterRole, the operator
kubectl -n nexo-operator rollout status deploy/nexo-operator
```

The operator watches every namespace; `WATCH_NAMESPACE` (comma separated) in `deploy/operator/manager.yaml`
limits it. The image is `docker.io/paolor/nexo-operator` (`Dockerfile.operator` builds it from the
sources); `make operator-build` builds the binary, which also runs outside the cluster with a kubeconfig.

## A cluster

```yaml
apiVersion: v1
kind: Secret
metadata: {name: lake-credentials, namespace: storage}
stringData:
  root-user: lake-admin
  root-password: change-me-please
---
apiVersion: nexo.r3n3r0.github.io/v1alpha1
kind: NexoCluster
metadata: {name: lake, namespace: storage}
spec:
  image: docker.io/paolor/nexo:1.0.0
  credentials: {secretName: lake-credentials}
  pools:
    - name: pool-0
      servers: 4
      volumesPerServer: 4
      storage: 100Gi
      storageClassName: fast-ssd
```

```bash
kubectl -n storage get nexo
NAME   PHASE   READY   IMAGE                         AGE
lake   Ready   4/4     docker.io/paolor/nexo:1.0.0   3m
```

The operator creates:

| Object | Name | Role |
|---|---|---|
| StatefulSet per pool | `lake-pool-0` | `servers` pods, `volumesPerServer` PersistentVolumeClaims each (`data1`..), started together |
| headless Service | `lake-hl` | the stable host names of the pods (`lake-pool-0-0.lake-hl.storage.svc.cluster.local`) |
| Service | `lake` | S3 API (port 9000) |
| Service | `lake-console` | console and Admin API (port 9001) |
| PodDisruptionBudget | `lake` | node drains take one pod of the cluster at a time |

Every pod runs `nexo server` with one endpoint per pool, in order
(`http://lake-pool-0-{0...3}.lake-hl.storage.svc.cluster.local:9000/data{1...4}`); the root credentials and
the KMS key are mounted from their Secrets. Other settings are `spec.env` (`NEXO_*` variables, see
[Configuration](configuration.md)); `resources`, `nodeSelector`, `tolerations` and `affinity` (default: the
pods of a pool spread on distinct nodes when possible) can be set per pool.

| Field | Meaning |
|---|---|
| `image`, `imagePullPolicy`, `imagePullSecrets` | the server image |
| `credentials.secretName` (`userKey`, `passwordKey`) | root credentials, keys `root-user` and `root-password` by default |
| `kms.secretName` (`key`) | master key of the builtin KMS (`NEXO_KMS_SECRET_KEY`), key `kms-secret-key` by default |
| `tls.secretName` | a `kubernetes.io/tls` Secret (optional `ca.crt`, as written by cert-manager): S3, console and internode traffic over TLS |
| `pools[]` | `name`, `servers`, `volumesPerServer`, `storage`, `storageClassName`, `resources`, `nodeSelector`, `tolerations`, `affinity` |
| `updateStrategy` | `Rolling` (default) or `AllAtOnce` |
| `env`, `resources`, `serviceType`, `clusterDomain`, `serviceAccountName`, `podLabels`, `podAnnotations`, `priorityClassName` | the rest of the pods and Services |

With TLS the certificate must name the pods (`*.lake-hl.storage.svc.cluster.local`) and the Services
(`lake.storage.svc.cluster.local`, `lake-console...` and the external names). The certificates are reloaded
by the servers when the Secret changes: no restart.

The status reports the phase (`Pending`, `Starting`, `Ready`, `Updating`, `Degraded`, `Invalid`), the ready
pods, the image every pod runs, the pools created and the last report of `/nexo/health/cluster`; the
conditions `Available`, `Progressing` and `Degraded` explain it (`kubectl describe nexo lake`).

## Updates

The StatefulSets use the `OnDelete` strategy: the operator, not Kubernetes, replaces the pods whose
template changed (a new image, new settings).

- **Rolling** (default): one pod at a time. A pod is replaced only when every pod is ready and
  `/nexo/health/cluster` reports every node and drive online with `maintenance: true` (one more node can stop
  without losing the write quorum of any erasure set); the next one once the new pod is ready and the
  cluster is healthy again. A cluster that cannot lose a node waits (`Progressing` reason `WaitingForQuorum`).
- **AllAtOnce**: every outdated pod together, a short outage. Needed to change the **major version**:
  nodes of different major versions do not form a cluster.

What every node must share is always changed on every pod together, whatever the strategy: the topology
(a new pool) and the content of the credentials and KMS Secrets. Changing the root credentials therefore
restarts the whole cluster.

The orchestrated update of the binary (`nexoctl admin update`) does not apply on Kubernetes: the image is the
binary, change `spec.image` instead.

## Expansion and pools

A pool is never resized: the API refuses to change `servers` or `volumesPerServer` of an existing pool.
The cluster grows by **appending** a pool to `spec.pools`; every pod restarts with the new topology (as
for an [expansion](operations/upgrade-expansion.md#expansion-with-a-new-pool) outside Kubernetes).

A pool is removed in two steps: decommission it first (`nexoctl admin decommission start ... <n>`, wait for
`complete`), then remove it from `spec.pools` and confirm it with the annotation:

```bash
kubectl -n storage annotate nexo lake nexo.r3n3r0.github.io/decommissioned-pools=pool-0
```

Without the annotation the operator refuses the change (phase `Invalid`) and keeps the pool running.

## The data

The operator never deletes a PersistentVolumeClaim: the StatefulSets retain their claims when they are scaled
or deleted, and deleting a `NexoCluster` deletes its StatefulSets, pods and Services but not the drives.
Recreating the cluster with the same name, namespace and pools finds them again. To free the storage, delete
the claims (`kubectl -n storage delete pvc -l nexo.r3n3r0.github.io/cluster=lake`) once the data is no
longer needed.
