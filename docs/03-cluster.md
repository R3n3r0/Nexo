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
- **Decommission** (P2): the pool is marked read-only, a distributed job moves every object (all versions)
  to the other pools, with persisted and resumable state; at the end the pool can be removed from the command line.
- **Rebalance** (P2): moves objects from the fullest pools to the least full ones up to a balance threshold.

## 3.9 Consistency

- **Strong read-after-write consistency** for PUT/DELETE of new objects and overwrites (thanks to lock + quorum).
- **Consistent** listing by merging the streams from the set's disks (ordered walk + listing quorum),
  with an optional listing cache for very large prefixes.
- Bucket/IAM metadata: written to quorum, then cache invalidation via the `peer` RPC; periodic reload as a safety net.

## 3.10 Multi-site (P2)

- **Bucket replication**: asynchronous, per rule (prefix/tag), active-active with last-writer-wins resolution on mod-time
  + version-id, explicit resync.
- **Site replication**: synchronizes IAM, policies and bucket configuration across N clusters; every created bucket is replicated.

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
