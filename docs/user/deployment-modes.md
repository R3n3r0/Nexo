# Deployment modes

The same binary (and the same Docker image) works in three modes, chosen **by the endpoints**
passed to `nexo server`, with no dedicated flags:

| Mode | Example command | Typical use |
|------|-----------------|-------------|
| **SNSD**: one node, one drive | `nexo server /srv/nexo` | development, testing, edge, small volumes |
| **SNMD**: one node, multiple drives | `nexo server /mnt/disk{1...4}` | a server with several drives, tolerance to drive failures |
| **Cluster**: multiple nodes, multiple drives | `nexo server http://nexo{1...4}.lan:9000/mnt/disk{1...4}` | production, high availability, growth by pools |

Rules: all local paths and only one → SNSD; all local paths and more than one → SNMD; at least one URL
→ cluster (then every endpoint must be a URL). The paths must be **absolute**.

All the features (S3, IAM, policies, presigned URLs, versioning, object lock, lifecycle, events, SSE,
distributions, cache, console, CLI) are available in every mode. What changes is data protection
and scalability:

| | SNSD | SNMD | Cluster |
|-|------|------|---------|
| Drive failure | left to the storage subsystem (RAID, cloud volume) | ✅ up to parity P per set | ✅ up to parity P per set |
| Node failure | ❌ | ❌ | ✅ (with enough nodes and quorum) |
| Bitrot | detected | detected and repaired | detected and repaired |
| Healing | — | ✅ | ✅ |
| Locks and RPC between nodes | local | local | distributed, quorum-based |
| Growth | migration | migration | adding pools |

The mode is written in `format.json` on every drive: restarting an SNSD drive with an erasure topology
(or vice versa) fails with an explicit error. `nexoctl admin info` and the **Cluster** page of the console
always show mode, pools, sets, nodes and drives.

## Ellipses

The endpoints use the `{start...end}` ellipses (three dots), expanded by Nexo itself:

| Form | Expansion |
|------|-----------|
| `/mnt/disk{1...4}` | `/mnt/disk1` … `/mnt/disk4` |
| `/mnt/disk{01...16}` | `/mnt/disk01` … `/mnt/disk16` (the leading zero fixes the width) |
| `http://node{a...d}:9000/data` | `http://nodea:9000/data` … `http://noded:9000/data` |
| `http://nexo{1...4}.lan:9000/mnt/disk{1...4}` | 16 endpoints, 4 per node |

- An argument with ellipses is **one pool**; several arguments with ellipses are several pools. Arguments
  without ellipses (`/mnt/a /mnt/b /mnt/c /mnt/d`) form a single pool; arguments with and without
  ellipses cannot be mixed.
- Bash does not expand `{1...4}` (with three dots); with other shells, or to be safe, quote the
  argument: `nexo server "/mnt/disk{1...4}"`.
- In a cluster every node runs **the same command** and recognizes its own endpoints from its host names
  or addresses and from the port of `--address`.

## SNSD: one drive

```bash
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersecret nexo server /srv/nexo
```

Objects are files on the file system (with `xl.meta` for metadata and versions), without erasure coding:
protection against failures is that of the underlying drive. Suitable for development, testing, edge and
volumes that are already redundant. Moving to SNMD or cluster requires a migration (see
[Backup and migration](operations/backup-migration.md)).

## SNMD: multiple drives on one node

```bash
nexo server "/mnt/disk{1...4}"
```

The drives form one or more **erasure sets** of 2–16 drives; every object is split into D data blocks and P
parity blocks (Reed-Solomon) spread over the drives of its set, with a checksum per block (bitrot). No
network involved: local locks, no RPC overhead.

## Cluster

```bash
# on each of the 4 nodes, identical
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD='<same on every node>' \
  nexo server "http://nexo{1...4}.lan:9000/mnt/disk{1...4}"
```

At startup the nodes look for each other (**bootstrap**): they check that topology, version and KMS
configuration match, wait for the drive quorum of every set and format the new drives. Until the bootstrap
is complete a node answers `503` (`/nexo/health/ready` too), so the nodes can start in any
order. The first node of the list formats the deployment as soon as it reaches the quorum; a node that
starts after the others have already formatted it waits a few seconds (INFO `empty local drives, formatting
them once settled`), formats its own drives, logs at INFO level `drive joined a fresh deployment, healing its
system configuration` and copies the configuration already written by the others: this is not a failure
(replaced drives of a deployment holding data are instead reported with a WARN, `empty local drives in an
existing deployment`).
A node started with arguments different from the majority does not join the cluster and exits with an error
that lists the disagreeing nodes and, for each of them, the endpoints present only on this node's command
line (`only on this node`) or only on the disagreeing node's (`only on that node`).

All the nodes are symmetric: any node serves any S3 or console request, so a load balancer goes in front
(see `deploy/docker/nginx.conf` and
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy)) with a health check on
`/nexo/health/ready`.

### How the sets are formed

Every pool is split into erasure sets of the same size: Nexo picks the largest size between 2 and 16
that divides the number of drives in the pool and spreads every set evenly across the nodes.

| Pool | Sets | Default parity | Drives per object (D+P) |
|------|------|----------------|-------------------------|
| 1 node × 4 drives | 1 × 4 | EC:2 | 2+2 |
| 1 node × 3 drives | 1 × 3 | EC:1 | 2+1 |
| 4 nodes × 2 drives | 1 × 8 | EC:4 | 4+4 |
| 4 nodes × 4 drives | 1 × 16 | EC:4 | 12+4 |
| 8 nodes × 4 drives | 2 × 16 | EC:4 | 12+4 |
| 6 nodes × 2 drives | 1 × 12 | EC:4 | 8+4 |

Check the result in advance with `nexo server --dry-run <endpoint>`:

```text
configuration OK: mode=distributed s3=:9000 console=:9001 endpoints=http://nexo{1...4}.lan:9000/mnt/disk{1...4}
  node: nexo1.lan:9000 (4 node(s))
  pool 1: 1 erasure set(s) of 16 drive(s), parity 4
  local drives: 4 writable, 0 formatted
```

### Choosing the parity

The default parity depends on the set size: 2 up to 5 drives, 3 for 6–7, 4 from 8 upward (never more
than half the drives). It is changed with `--storage-class-standard EC:n` (`NEXO_STORAGE_CLASS_STANDARD`); the
`REDUCED_REDUNDANCY` class has parity 1 when not configured (`--storage-class-rrs`, at most that of
`STANDARD`). A client chooses the class per object with the S3 header `x-amz-storage-class`.

| Set of N drives, parity P | Effect |
|---------------------------|--------|
| Usable capacity | (N − P) / N of the raw capacity |
| Reads | possible with at least D = N − P drives of the set |
| Writes | possible with at least D drives (D + 1 when D = P, to avoid split-brain) |
| Locks (cluster) | writes with N/2 + 1 reachable nodes |

More parity = more failures tolerated and less capacity. Examples:

- **4 nodes × 4 drives, EC:4**: 75% usable capacity; up to 4 arbitrary drives, or a whole node, can be
  lost without interruption.
- **4 nodes × 2 drives, EC:4**: 50% usable capacity; with one node down reads and writes continue; with
  two nodes down reads are still possible but writes are not (data and lock quorum).
- **SNMD 4 drives, EC:2**: 50% usable capacity; writes need 3 drives out of 4 and reads need 2.

Every version remembers the parity it was written with: changing the class applies to new objects.

### Sizing

- **The same drives** in number and size on every node of a pool: the usable space of a set is
  limited by the smallest drive.
- Dedicated drives formatted with **XFS**, mounted with `noatime`, **no RAID** under Nexo (erasure coding
  provides the redundancy) and no LVM joining several drives.
- To keep writing with a node down you need at least **3 nodes** (locks require N/2 + 1 nodes)
  and a parity at least equal to the drives a node has in each set (with the defaults this holds from
  3 nodes × 4 drives and from 4 nodes × 2 drives upward); with 2 nodes, shutting one down blocks writes.
  4 or more nodes leave room for maintenance.
- Plan for free space: new objects avoid pools above 95%; the `NexoCapacityHigh` alert fires
  at 85% (see [Monitoring](operations/monitoring.md)).
- The distributions cache (`--cache-dir`) goes on a separate drive (SSD/NVMe), never on a data drive.
- To measure drives and network before going to production: `nexoctl admin speedtest local drive|object|net`.

### Network

| Port | Used by | Notes |
|------|---------|-------|
| 9000 | S3 clients, load balancer, **all the other nodes** (RPC on `/nexo/rpc/v1/`) | must be open between the nodes |
| 9001 | browser (console), `nexoctl admin`, Admin API | not needed between the nodes |

- Every host name of the endpoints must resolve to the node's address **on every node** (DNS or `/etc/hosts`).
- Traffic between nodes is HTTP/2 authenticated by short-lived tokens bound to the single call; with
  `http://` endpoints it travels in clear text (the node warns about it at startup): use it only on a
  trusted private network.
- The public proxy must never forward `/nexo/rpc/` (the configurations in `deploy/proxy` block it).
- Synchronized clocks (NTP): a node more than 2 s off is reported in the logs and in
  `nexoctl admin info`; signed requests tolerate 15 minutes.

### TLS between nodes

1. Prepare on every node a directory with `public.crt` and `private.key`, with a certificate covering the
   endpoint names (`nexo1.lan` … `nexo4.lan`) and the public names, and the trusted CAs in `CAs/`.
2. Start every node with `NEXO_CERTS_DIR` (or `--certs-dir`) and `https://` endpoints:

   ```bash
   NEXO_CERTS_DIR=/etc/nexo/certs nexo server "https://nexo{1...4}.lan:9000/mnt/disk{1...4}"
   ```

3. For mutual authentication add `--tls-internode-verify` (`NEXO_TLS_INTERNODE_VERIFY=true`): calls
   between nodes without a client certificate verified against the CAs in `CAs/` are rejected (the
   certificate must allow client usage).

Details and certificate rotation in [TLS and certificates](security/tls.md).

## Expansion with a pool

A cluster grows by **adding a pool** (a group of nodes and drives), without moving data:

```bash
# before: one pool
nexo server "http://nexo{1...4}.lan:9000/mnt/disk{1...4}"
# after: two pools, the same command on all 8 nodes
nexo server "http://nexo{1...4}.lan:9000/mnt/disk{1...4}" "http://nexo{5...8}.lan:9000/mnt/disk{1...4}"
```

Existing objects stay in their pool (overwrites too); new objects go to the pools in
proportion to their free space. The complete procedure is in
[Upgrade and expansion](operations/upgrade-expansion.md#expansion-with-a-new-pool).
To spread the existing objects over the new pool, or to retire a pool, see
[Rebalancing](operations/upgrade-expansion.md#rebalancing-between-pools) and
[Decommissioning a pool](operations/upgrade-expansion.md#decommissioning-a-pool).

## From one mode to another

| From → To | How |
|-----------|-----|
| SNSD → SNMD or cluster | new deployment + `nexoctl admin cluster export/import` + `nexoctl mirror --preserve` |
| SNMD → cluster | as above |
| Cluster → larger cluster | add a pool, no migration |

See [Backup and migration](operations/backup-migration.md).
