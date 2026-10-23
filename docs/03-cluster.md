# 3. Clustering

## 3.1 Chosen model

Nexo adopts a **shared-nothing model with a static per-pool topology** (similar to MinIO):

- A **deployment** consists of one or more **server pools**.
- A **pool** is a set of nodes × disks defined at startup with ellipsis expansion:
  ```bash
  nexo server http://node{1...4}.example.com:9000/mnt/disk{1...4}
  # expansion: a second pool is added
  nexo server http://node{1...4}.example.com:9000/mnt/disk{1...4} \
              http://node{5...8}.example.com:9000/mnt/disk{1...8}
  ```
- Each pool is split into **erasure sets** of a fixed size (2–16 disks), chosen as the greatest
  common divisor that spreads the disks evenly across the nodes (a set uses disks on different nodes).
- Each object belongs to **exactly one erasure set** of **exactly one pool**.
- All the nodes are **symmetric**: any node accepts any S3 request (behind a load balancer).
- **No master node, no metadata DB**.

### Why not dynamic consistent hashing?
| Aspect | Static pools (chosen) | Consistent hashing + dynamic membership |
|--------|-----------------------|-----------------------------------------|
| Complexity | Low | High (gossip, rebalancing, ring versions) |
| Expansion | A pool is added, no data movement | Proportional data movement |
| Performance predictability | High | Rebalancing affects traffic |
| Growth granularity | Per pool (group of nodes) | Per node |

Rebalancing between pools remains available as an explicit operation (S-STO-10).

## 3.2 Placement

1. **Pool selection** (new objects only):
   - if the object already exists (any version) in a pool → same pool;
   - otherwise a random choice weighted by free space, excluding pools being decommissioned or > 95% full.
2. **Set selection**: `set = SipHash-2-4(objectKey, deploymentID) mod numSets`.
3. **Shard distribution**: a permutation of the set's disks derived from `crc32(key)` to balance
   data/parity across the disks.

## 3.3 Erasure coding and quorum

For a set of **N** disks with parity **P** (default: `N ≤ 5 → P=2`, `6–7 → 3`, `≥ 8 → 4`) and data **D = N − P**:

| Operation | Quorum |
|-----------|--------|
| Read | `D` disks with consistent shards |
| Write | `D` disks (`D+1` if `D == P`, to avoid split-brain) |
| Lock | `N_nodes/2 + 1` of the lock servers involved |

Example: 4 nodes × 4 disks = 16 disks, 1 set of 16 with EC:4 → tolerates the loss of 4 disks (or 1 whole node)
for reads and writes; usable capacity 75%.

## 3.4 Internode communication

- Transport: HTTP/2 on the same S3 port under `/nexo/rpc/v1/`, optional (recommended) mTLS.
- Authentication: short-lived JWT (20 s) signed with a key derived from the root credentials (HMAC) + deployment ID
  check; nodes with an incompatible version/format are rejected. The signature is verified before any
  claim; the token is bound to the target node (`aud`) and to **a single call**: method and target (path and
  query, `htm`/`htu`), SHA-256 hash of the arguments (`bh`: the msgpack body, or the first frame of an upload,
  checked by the routes) and a unique id (`jti`) accepted only once (id cache until expiry);
  an intercepted token is valid for no other call, nor for repeating the same one. The tolerated skew is
  15 s; error responses never reveal the deployment ID.
- Binding to the deployment: a node with formatted disks reads the deployment ID from its own `format.json`
  **before opening the listener**; a node with empty disks binds as soon as it learns it: when a peer
  writes its `format.json` or when its bootstrap reads or generates it (the node that formats the deployment
  switches to the new ID before writing the formats on the peers). A caller without a deployment ID (bootstrap)
  can reach only the bootstrap calls (ping, `bootstrap/*`, read and stat of `format.json` without glob,
  disk state: no listing of volumes or objects); until a node knows its own deployment
  it accepts from any caller only those same calls plus the write of `format.json` and of the healing
  tracker. This restriction protects against configuration mistakes (a node of another deployment with the
  same root credentials), not against an attacker: whoever holds the root credentials can always obtain the
  deployment ID (`bootstrap/config`) and sign valid tokens.
- Rejections: every rejection (token, deployment, version, client certificate) is logged at Warn level with
  remote address and path (never the token), at most once per address and reason per minute, and counted in
  `nexo_rpc_auth_refused_total{reason}`; an address that accumulates 20 authentication failures is
  rejected without verification (429) until its credit refills (one every 3 s): the server is not a fast
  oracle for the root password.
- Transport: HTTP/2 only. With `http` endpoints the traffic and the tokens are in clear text (warning at startup): the
  token binding limits passive eavesdropping, not an active attacker, so TLS is required on
  untrusted networks. With `--tls-internode-verify` (`NEXO_TLS_INTERNODE_VERIFY`, `https` endpoints) TLS is
  **mutual**: each node presents its own certificate (`public.crt`, with client usage) and internode calls
  without a client certificate verified against the CAs in `certs/CAs` are rejected (S3 requests on the same
  port present none). We chose not to require an opt-in flag for `http` endpoints
  (the warning remains), so as not to break deployments on private networks. The public proxy never forwards
  `/nexo/rpc/`, and a virtual-host request (`<bucket>.<domain>/nexo/rpc/v1/...`) is an S3 key, not an
  internode call. Unary peer responses are limited to 64 MiB.
- RPC services:
  | Service | Purpose |
  |---------|---------|
  | `storage` | Remote disk access (ReadVersion, WriteMetadata, CreateFile stream, ReadFileStream, RenameData, Walk, DiskInfo, Delete…) |
  | `lock` | Lock/Unlock/RLock/RUnlock/Refresh/ForceUnlock |
  | `peer` | Cache invalidation (IAM, bucket metadata, config), server info, trace/log stream, signal (restart), local metrics |
  | `bootstrap` | Consistency check of the startup configuration across nodes |
- Encoding: msgpack; binary streams for data with framing and checksums.
- Connection pooling, per-call timeout, circuit breaker for offline disks/nodes with background reconnection.

## 3.5 Cluster bootstrap

1. Each node expands the endpoints and checks that the list is identical on all the nodes (topology hash via the `bootstrap` RPC).
2. Wait until at least the **disk quorum** of each set is reachable (retry with backoff, clear log of what is missing).
3. If no `format.json` exists → the node with the first endpoint generates the format (deployment ID, set/disk map) and
   writes it to all the disks; the others wait.
4. New/empty disks in an existing format → marked for **healing** and repopulated.
5. IAM/config loaded from the system bucket, background services started (scanner, heal, ILM, replication) with
   **lightweight leader election** through a distributed lock (`leader` lock with lease) for the jobs that must run only once.

## 3.6 Distributed locks (dsync)

- Each node exposes an in-memory lock server.
- A lock is acquired if `≥ n/2+1` lock servers answer positively within the timeout; otherwise release and retry with jitter.
- **Lease + refresh** every 10 s: if the refresh loses quorum, the operation is cancelled (context cancel).
- "Orphan" locks expire automatically after 1 min without refresh.
- Multiple concurrent read locks, exclusive write locks.
- `admin lock list/clear` for diagnostics.

## 3.7 Healing

| Mechanism | Trigger | Action |
|-----------|---------|--------|
| Heal-on-read | A read detects a missing/corrupted shard (bitrot) | Rebuilds the response from the remaining shards, queues a heal |
| MRF (Most Recently Failed) | Write succeeded with quorum but not on all the disks | In-memory queue (persisted periodically) retried in the background |
| Scanner | Cyclic, with adaptive throttling | Usage computation, ILM application, sampled shard verification, heal |
| Disk replacement | New disk without `format.json` | Full healing of the disk with checkpoint (`healing.bin`) and resume after restart |
| Manual | `nexoctl admin heal` | Heal of bucket/prefix, `--deep` for full bitrot verification |

- An `xl.meta` that cannot be decoded does not block the disk: at the first write or delete it is moved to
  quarantine (`.nexo.sys/quarantine/`, never cleaned automatically, for analysis) and treated as absent;
  heal-on-read, MRF, `HealObject` and disk healing rewrite it from the copy on the other disks with all
  the versions.
- Writes and deletes rejected for lack of quorum are rolled back on the disks that had
  applied them (previous version restored), so as not to leave a diverging minority.

## 3.8 Expansion, decommission, rebalance

- **Expansion**: all the nodes are restarted with the new pool list (rolling restart). The new pools are
  formatted; new objects are spread weighted by free space.
- **Decommission** (P2): the pool is marked *draining* on every node (control document in the system store,
  polled every 5 s): it takes no new object, version or multipart upload, a key it holds is overwritten in
  another pool, reads still find it. A job on the leader walks the pool and moves every key with all its
  versions (delete markers included) in three phases: snapshot of the versions under the read lock, copy of
  the stored bytes (still encrypted/compressed) to a temporary area of the target and read-back with a
  SHA-256 comparison without lock, then under the write lock, in batches, re-check that the source did not
  change, commit, verify the metadata and only then delete from the source. A changed or failed key stays in
  the pool and is retried by the next pass; passes repeat until one finds nothing, then the job waits for the
  pool's multipart uploads. Progress is persisted and resumes after a restart or a leader change. The first
  pool cannot be decommissioned: it holds the system documents. At the end the pool is removed from the
  command line.
- **Rebalance** (P2): moves keys with the same procedure from the pools above the average usage by more than
  5 points to the pools below it, rechecking the usage every few keys, until the sources are near the average.

## 3.9 Consistency

- **Strong read-after-write consistency** for PUT/DELETE of new objects and overwrites (thanks to lock + quorum).
- **Consistent** listing by merging the streams from the set's disks (ordered walk + listing quorum),
  with an optional listing cache for very large prefixes.
- Bucket/IAM metadata: written to quorum, then cache invalidation via the `peer` RPC; periodic reload as a safety net.

## 3.10 Multi-site (P2)

- **Bucket replication** (S-BKT-12): asynchronous, per rule (prefix/tag, priority), active-active. A write that a
  rule selects is stored with `x-amz-replication-status: PENDING` and one internal state per target, then the events
  decorator queues one task per target in a persistent per-node queue (`.nexo.sys/replication` on the first drive,
  FIFO per key). Workers read the version and PUT it to the remote with the replica headers (`X-Nexo-Replica*`:
  version id, mod-time, ETag, delete marker), which the remote accepts with `s3:ReplicateObject`/`ReplicateDelete`/
  `ReplicateTags` and stores as `REPLICA` without queueing it again (no ping-pong). The version keeps its id and
  mod-time on both sides, so xl.meta orders concurrent writes the same way everywhere: last writer wins on mod-time.
  Failures are retried with exponential backoff (1 s to 5 min, 30 attempts) and end as `FAILED`. Targets (endpoint,
  bucket, sealed credentials, ARN `arn:nexo:replication::<id>:<bucket>`) live in the system store; a resync job on
  the leader walks the versions, HEADs the remote and queues what it lacks.
- **Site replication** (S-STO-11): the sites of a group share the root credentials and call each other's Admin
  API with them (`/site-replication/peer/...`, root only); the entries they exchange are sealed with a key derived
  from them. The leader of each site runs a round every 5 s with every other site: it compares the hashes of the
  replicated entries (users, groups, access keys as stored by IAM, named policies, bucket settings) of both sides
  with the base both had after the last round (`site-replication/base/<peer>.json`), pushes what changed here,
  pulls what changed there, and on a double change keeps the newest (`updatedAt`; else the smallest deployment
  ID), a change beating a deletion. Without a base nothing is deleted. Each round then gives every bucket
  versioning and a bucket replication rule `site-<name>` to every other site (resync of the existing objects, one
  target at a time per bucket). Not replicated: configuration, KMS keys, notification targets, distributions,
  STS credentials and sessions.

- **Remote tiers** (S-STO-12, §3.12).

- **Updates and coordinated restarts** (S-OPS-09, §3.13).

## 3.11 Failure scenarios to support

| Scenario | Expected behavior |
|----------|-------------------|
| 1 disk offline | Reads/writes OK, heal when it comes back |
| P disks offline in a set | Reads/writes OK (writes if the D quorum is met) |
| > P disks offline | `503 SlowDownWrite`/`SlowDownRead` error (`XNexoLockTimeout` without lock quorum) for that set, other sets working |
| 1 node offline (4 nodes) | Cluster fully operational |
| 2+2 network partition | No write possible without lock quorum (no split-brain); reads if the data quorum is reachable |
| Corrupted disk (bitrot) | Detected on read, correct response, shard repaired |
| Crash during a write | No partial object visible; tmp files cleaned at startup and, while serving, by the janitor (`.nexo.sys/tmp` entries older than 24 h, checked every hour) |
| Crash during multipart completion | Completion first writes a journal (`completing.json`) in the upload directory and moves the parts to a staging directory; on the next attempt the upload is concluded (version already committed) or the parts go back to their place: the upload stays completable |
| Multipart parts on different disks | Each part requires `D` disks holding it intact; the disks that lack it receive it rebuilt from the shards before the commit |
| Clock skew between nodes | Warning if > 2 s; SigV4 tolerates 15 min; locks independent of the clock |

## 3.12 Remote tiers

The `Transition` actions of the lifecycle move the data of versions to remote tiers: S3 buckets (endpoint,
bucket, prefix, sealed credentials in the system store, `tiers/config.json`). A transition copies the stored
bytes of the version (encrypted and compressed as on the drives) to the tier as one object
`<prefix><deployment id>/<uuid v7>`, its metadata naming the version, under the read lock of the key, and checks
its size. Then, under the write lock and if the version is still the same, it records the tier in the metadata
of the version (`UpdateMetadata`, local data kept), then writes the version without data directory
(`WriteMetadata`, local data released). The version keeps its parts, sizes and checksums; its reads fetch ranges
of the tier object (never pinned: the read lock is held until the body is closed). The drives agree on a
transitioned version whatever the state of its local data (the quorum key names the tier object instead of the
data directory), and the healing releases the data left by an interrupted transition. A pool move carries the
metadata only. The leader sweeps the tiers every hour: an object of this deployment older than one hour whose
version, looked up under its lock, does not name it is removed. A tier holding objects of this deployment
cannot be removed. A copy onto itself stores the data on the drives again.

## 3.13 Orchestrated update and coordinated restart

Every node runs an update agent, reached by its peers over the internode RPC (`/update/state|stage|activate|restart|stop`).
The node receiving `POST /service/update` (or `POST /service/cluster/restart`) orchestrates the job, kept in the
system store (`update/job.json`): one job at a time.

- **Staging** (update only): every node downloads the binary into the directory of the running one, verifies its
  SHA-256, runs it (`version --json`, the version and the platform) and keeps it as `.nexo-update-<digest>`. Any
  failure, or two versions, fails the job before any node changes; a rolling update refuses a change of major
  version (the nodes would not form a cluster).
- **Activation**: the other nodes first, sorted, this one last. Rolling: a node only once
  `Health(Maintenance)` holds (one more node can stop without losing the write quorum of any set), the next one
  once the node answers with a new run (`boot`) and, for an update, the new version (up to 5 minutes, else the job
  fails and the nodes after it are untouched). All at once: every node without waiting. An update renames the
  running binary `<binary>.old` and the staged one in its place (a failed rename puts it back), then the process
  stops gracefully and replaces itself (`execve` on Unix; on Windows it starts its successor and exits); if the new
  binary cannot be executed, `.old` is put back and executed. A restart restarts the server in its process.
- **Completion**: the orchestrator records itself `restarting` before its own activation; its next run, or any
  node reading the job once the orchestrator runs again, completes it: `completed` when every node is back (with
  the version), `failed` when a node is on another version or the job was interrupted before activating it.
- **Coordinated stop**: `POST /service/cluster/stop` stops the other nodes, then the one answering.

Only a server run as a process by its binary (`nexo server`) can update or restart itself; embedded (the tests) the
calls answer `501`. The binary directory must be writable by the server: packages and images are updated by their
own tools.
