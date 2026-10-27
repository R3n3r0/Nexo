# Upgrade and expansion

## Upgrading a single node (SNSD, SNMD)

1. Read the release notes and verify the release ([Installation](../installation.md#binaries-archives)).
2. To be safe, export IAM and configuration: `nexoctl admin cluster export local --output backup.zip`
   (see [Backup and migration](backup-migration.md)).
3. Replace the binary (or the package, or the image tag) and restart the service:

   ```bash
   sudo apt install ./nexo_1.0.1_amd64.deb     # or: install -m 0755 nexo /usr/local/bin/nexo
   sudo systemctl restart nexo
   nexo healthcheck --ready && nexo version
   ```

The restart stops the server gracefully (`SIGTERM`: new requests get `503`, the ones in flight
complete within the shutdown timeout) and interrupts the service for a few seconds.

## Rolling upgrade of a cluster

Nodes with the same **major version** coexist in the same cluster: you upgrade one node at a time
without interrupting the service, provided the quorum holds with one node down (see
[Deployment modes → Choosing the parity](../deployment-modes.md#choosing-the-parity)). A node with a major
version different from the majority does not join the cluster.

For each node, one at a time:

1. Check that a node can be shut down: `/nexo/health/cluster` must answer `"maintenance": true`
   (and `writeQuorum: true`):

   ```bash
   curl -s http://nexo1.lan:9000/nexo/health/cluster
   ```

2. Install the new binary and restart the service on the node:

   ```bash
   sudo apt install ./nexo_1.0.1_amd64.deb && sudo systemctl restart nexo
   ```

   While stopping, the node answers `503` to `/nexo/health/ready`: the load balancer excludes it and
   reads are retried on another node (the configurations in `deploy/proxy` already do this).

3. Wait until it is ready and in sync again:

   ```bash
   nexo healthcheck --ready                     # on the node; with TLS: --certs-dir /etc/nexo/certs
   nexoctl admin info local                     # every node online, the new version on the upgraded node
   ```

4. Move on to the next node. The writes that arrived while the node was down are completed by the healing
   queue (MRF) when it comes back.

With orchestrators:

- **Kubernetes (Helm)**: `helm upgrade` with the new tag updates the StatefulSet one pod at a time; the
  PodDisruptionBudget (`maxUnavailable: 1`) protects the quorum during drains.
- **Kubernetes (operator)**: change `spec.image` of the `NexoCluster`; the operator replaces one pod at a time,
  each only while the cluster can lose a node ([Kubernetes operator](../kubernetes-operator.md)).
- **Docker Swarm**: `docker service update` with `parallelism: 1` (already set in `deploy/swarm/stack.yml`).
- **Docker Compose**: `docker compose up -d --no-deps nexo1`, wait for `healthy`, then `nexo2`, …

## Orchestrated update (`nexoctl admin update`)

When every node runs the binary of a release archive (not a package, not an image), one command updates the
whole deployment, one node at a time, without downtime:

```bash
sha256sum nexo-v1.1.0-linux-amd64                  # or the checksums file of the release
nexoctl admin update --sha256 3f5a... cluster https://dl.example.com/nexo-v1.1.0-linux-amd64
```

The node that answers the request orchestrates the job:

1. **Staging, all or nothing.** Every node downloads the binary into the directory of the one it runs,
   verifies its SHA-256 and runs it (`nexo version --json`) to read its version and check its platform.
   If a node fails, or the nodes do not agree on the version, nothing changes on any node.
2. **Rolling activation.** Node by node (the node answering last), once `/nexo/health/cluster` would allow
   a maintenance (one more node can stop without losing the write quorum of any erasure set): the staged
   binary takes the place of the running one, which is kept as `nexo.old` next to it, and the process
   replaces itself with it once the requests in flight are answered (same PID, arguments, environment
   and log files). The next node waits until this one runs again with the new version.
3. The command follows the progress and returns when every node runs the new version; through a load
   balancer the polling survives the restart of the node answering. `--no-wait` returns at once,
   `nexoctl admin update --status cluster` shows the last job.

`--all-at-once` restarts every node together (an outage of a few seconds): it is required to change the
**major version**, since nodes of different major versions do not form a cluster. A node that does not
come back within 5 minutes stops the job, the nodes after it keep their binary: fix the node (its previous
binary is `nexo.old`) and start the update again. On Unix, if the new binary cannot even be executed, the
process puts `nexo.old` back and runs it. The update needs `admin:ServerUpdate` (`admin:ServerInfo` for the
status), the Admin API on the nodes and a binary directory **writable by the user of the server**: with
the packages (`/usr/bin`, owned by root) keep updating with the package manager. In a container or on
Kubernetes the image is the binary: change the image tag instead (`helm upgrade`), which rolls the pods the
same way.

## Restart without upgrading

```bash
nexoctl admin service restart local          # asks for confirmation; --yes in scripts
```

Restarts the services of the **node that answers** the alias's Admin API within the same process (same
binary and same static configuration): useful after a problem, not to install a new version.
`nexoctl admin service stop` stops the process (it must be restarted by hand or by the supervisor). They
require `admin:ServiceRestart` and `admin:ServiceStop`.

`--cluster` acts on **every node** of the deployment:

```bash
nexoctl admin service restart --cluster cluster                  # one node at a time, the quorum kept
nexoctl admin service restart --cluster --all-at-once cluster    # every node together
nexoctl admin service stop --cluster cluster                     # the other nodes, then the one answering
```

The rolling restart follows the same rules as the orchestrated update (the next node once the previous
one runs again and the deployment can lose a node) and is followed the same way (`nexoctl admin update
--status`).

From the console, **Cluster → Maintenance** starts the same update (binary URL and SHA-256), the
coordinated restart (rolling or all at once) and the coordinated stop (confirmed by typing a word), and
shows the last job node by node while the nodes restart.

Reloading the TLS certificates needs no restart: `systemctl reload nexo` (`SIGHUP`) or just
replacing the files (see [TLS](../security/tls.md)).

## Expansion with a new pool

A cluster grows by adding a **pool**: a group of nodes (possibly with drives of a different size
from the first pool). Existing data is not moved.

1. Prepare the new nodes like the first ones: same Nexo version, same root credentials, same
   KMS configuration, drives mounted and empty, names resolvable from every node, port 9000 open between all
   of them.
2. Work out the new command line: one argument with ellipses per pool, **in the same order** on every
   node (the existing pool stays first):

   ```bash
   NEXO_ENDPOINTS="http://nexo{1...4}.lan:9000/mnt/disk{1...4} http://nexo{5...8}.lan:9000/mnt/disk{1...4}"
   ```

   Check it on a new node with `nexo server --dry-run` (expected pools, sets and parity).
3. Stop **all** the nodes of the cluster, update `NEXO_ENDPOINTS` (or the arguments) on every node, old and
   new, and restart them all. During the bootstrap the nodes check that they have the same topology: a node
   left with the old command line does not join the cluster. This is a short, planned interruption.
4. The drives of the new pool are formatted with the same deployment ID. Verify with
   `nexoctl admin info local`: two pools, every set `ok`.

The console guides the same steps: **Cluster → Pool management → Expand cluster** checks the new pools on
the server exactly as the nodes will parse them (`POST /api/v1/pools/plan`: erasure sets, parity, new
nodes, duplicate or invalid drives refused), gives the new `NEXO_ENDPOINTS` of every node to copy, then,
once the checklist is confirmed, stops the cluster. The same wizard removes the pools whose decommission is
complete. Every pool must already be written as one argument with ellipses.

After the expansion:

- the objects already present stay in their pool, even when they are overwritten;
- new objects go to the pools in proportion to their free space (pools above 95% are excluded);
- to spread the existing objects over the new pool too, start a [rebalance](#rebalancing-between-pools).

On Kubernetes the expansion is `distributed.pools` in the chart (here too with a restart of every pod); with
Docker Compose or Swarm you add the services of the new nodes and change `command` on **all** of them.

## Rebalancing between pools

```bash
nexoctl admin rebalance start cluster
nexoctl admin rebalance status cluster      # objects and bytes moved, the pool being emptied
nexoctl admin rebalance stop cluster
```

The rebalance moves objects, with all their versions, from the pools fuller than the average usage (by more
than 5 points) to the pools below it, until every pool is close to the average. It runs on the leader node in
the background, survives restarts (its progress is in the system documents) and can be stopped at any time:
the objects moved so far stay where they are. It needs `admin:Rebalance`; the console shows it under
**Cluster → Pool management**.

## Decommissioning a pool

A pool is removed in two steps: first its objects are moved to the other pools, then it is removed from the
command line.

```bash
nexoctl admin decommission status cluster      # pools, usage, decommission progress
nexoctl admin decommission start cluster 2     # the 1-based position of the pool in the command line
nexoctl admin decommission cancel cluster 2    # the pool takes new objects again
```

1. Check that the other pools have enough free space for the objects of the pool (a pool above 95% takes
   nothing).
2. Start the decommission. From then on the pool takes no new object, version or multipart upload: they go
   to the other pools. Reads keep working while the objects move.
3. A background job on the leader walks the pool bucket by bucket and moves every version of every object
   (delete markers included) to another pool. Each copy is read back and compared before the copy in the
   pool is removed, under the lock of the key: an object written meanwhile is left in place and moved by the
   next pass. Passes repeat until one finds nothing left; objects that could not be moved are retried (up
   to 10 passes, then the job reports `failed` and the pool keeps them). Multipart uploads started before
   the decommission complete in the pool and are moved afterwards.
4. When the status reports `complete`, stop all the nodes, remove the pool from the command line
   (`NEXO_ENDPOINTS`) of **every** node and restart them, as for an expansion.

The progress survives restarts and a change of leader. It needs `admin:Decommission`; the console shows it
under **Cluster → Pool management**.

The **first pool** of the command line cannot be decommissioned: it holds the system documents (IAM,
configuration, bucket metadata). To retire its nodes, move the data to a new deployment with
`nexoctl admin cluster export/import` and `nexoctl mirror --preserve`
([Backup and migration](backup-migration.md)).

## Before upgrading: checklist

- [ ] backup of IAM and configuration (`admin cluster export`);
- [ ] healthy cluster: every node online, no degraded set, empty MRF queue (`nexoctl admin info`);
- [ ] free space and inodes on the drives;
- [ ] synchronized clocks;
- [ ] same KMS configuration on every node (a node with a KMS different from the majority does not start).
