# Healing and disks

In the modes with erasure coding (SNMD and cluster) Nexo repairs missing or corrupted data by itself,
rebuilding it from the other drives of the set. In SNSD there is no redundancy: bitrot is detected (the read
fails instead of returning wrong data) but not repaired.

## How it works

| Mechanism | When it starts | What it does |
|-----------|----------------|--------------|
| Heal on read | a read finds a missing or corrupted block | answers with the rebuilt data and queues the repair |
| MRF queue | a write succeeded with the quorum but not on every drive (drive or node down) | retries the repair in the background, with growing waits up to `NEXO_HEAL_RETRY_MAX_DELAY` (5 min) |
| Scanner | cyclic (`scanner.cycle`, default 1 min pause between cycles) | computes usage, applies the lifecycle, verifies a sample of the data and queues repairs |
| New drive | an empty drive in place of one of the deployment (checked every 30 s) | formats it and repopulates it with every object, with checkpoints to resume after a restart |
| Manual | `nexoctl admin heal` or the **Healing** page of the console | repairs a bucket, a prefix or everything, optionally with a full bitrot check |

An unreadable `xl.meta` does not block the drive: it is moved to `.nexo.sys/quarantine/` (never cleaned up
automatically, for analysis) and rewritten from the copies on the other drives. Writes rejected for
lack of quorum are rolled back on the drives that had applied them.

The status is visible in `nexoctl admin info` (`background healing` line per node, drives being healed), on
the **Cluster** page of the console and in the metrics `nexo_mrf_queue_*`, `nexo_heal_*`,
`nexo_drive_healing`.

## Manual healing

```console
$ nexoctl admin heal -r --dry-run local/data          # check only, no changes
Heal 2e78a33e-…: done, scanned 6, healed 1, failed 0, 976.6KiB
  drive /mnt/disk3 (pool 1 set 1 disk 3) online: missing 1, corrupt 0, offline 0, healed 0
  object data/f1.bin: disk 3 missing -> missing

$ nexoctl admin heal -r local/data                    # repair
Heal 3daac8d1-…: done, scanned 6, healed 1, failed 0, 976.6KiB
  drive /mnt/disk3 (pool 1 set 1 disk 3) online: missing 1, corrupt 0, offline 0, healed 1
  object data/f1.bin: disk 3 missing -> ok
```

| Command | Effect |
|---------|--------|
| `nexoctl admin heal local` | drive format and metadata of every bucket |
| `nexoctl admin heal -r local` | every object of every bucket |
| `nexoctl admin heal -r local/photos/2026/` | the objects under a prefix |
| `nexoctl admin heal -r --deep local/photos` | verifies the checksum of **every** block (reads all the data: slow) |
| `nexoctl admin heal -r --remove local/photos` | deletes "dangling" objects, present on too few drives to ever be read again |
| `nexoctl admin heal --background -r local` | starts the job and prints its id without waiting |
| `nexoctl admin heal status local [ID]` | status of the running and recent jobs |

The exit code is 1 if the job fails or if some object cannot be repaired. Requires `admin:Heal`.

## Replacing a drive

The new drive must be mounted **at the same path** as the old one, empty, writable by the service
user. No restart is needed: within 30 seconds the node recognizes it, formats it and repopulates it.

1. Find out which drive failed: `nexoctl admin info local` (status `offline` or errors), metric
   `nexo_drive_errors_total`, alert `NexoDriveOffline`.
2. Check that the set still has its quorum (`/nexo/health/cluster`: `writeQuorum: true`).
3. Replace the drive, format it (XFS) and mount it at the same mount point, with the right owner:

   ```bash
   sudo mkfs.xfs -f /dev/sdc
   sudo mount -o noatime /dev/sdc /mnt/disk3          # and update /etc/fstab (UUID)
   sudo chown nexo:nexo /mnt/disk3 && sudo chmod 0750 /mnt/disk3
   # Docker: sudo chown 65532:65532 /mnt/disk3
   ```

4. The node logs show `heal: empty drive formatted and marked for healing`, then `heal: healing
   drive` and finally `heal: drive healed` with the number of objects and bytes restored. During the
   repopulation the drive shows as `healing` in `admin info` and in the console.
5. If the node does not detect the drive (for example it had been removed from the mount configuration while
   the process was running), restart that node only: at startup empty drives are formatted and
   repopulated, and an interrupted repopulation resumes from its checkpoint.

In a cluster the procedure is the same on the node that hosts the drive; to replace **all** the drives of a
node (reinstalled node) stop the node, prepare the empty drives and restart it: it rejoins the cluster and
repopulates itself.

> **Warning**: never copy the files of a drive to another one by hand, and never reuse a drive of another
> deployment without wiping it: a `format.json` of another deployment is rejected.

## Node down or unreachable

As long as the quorum holds, a node that is down does not interrupt the service: the writes land on the
other drives and the MRF queue completes them when the node comes back. No action is needed when it is
powered on again; to check that everything has caught up:

```bash
nexoctl admin info local                 # every node online, MRF queue at zero
nexoctl admin heal -r --dry-run local    # no object to repair
```
