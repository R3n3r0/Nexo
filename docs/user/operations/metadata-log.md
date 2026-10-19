# Metadata log (faster small writes)

By default every object has a directory on each drive with an `xl.meta` file: a PUT of a small object creates a
directory and a file, flushes them and renames them into place. On drives with a slow flush (desktop SSDs and HDDs,
virtual disks, Windows) this caps the throughput of small writes.

With the **metadata log** the metadata of the objects goes into append-only segments on each drive. Concurrent
writes share one flush, and an index in memory locates each object. Small objects (up to 128 KiB, kept inline)
then need no directory at all; large objects keep their data in data directories.

| Measure (4 KiB PUT, warp, concurrency 20, single drive) | xl.meta files | metadata log |
|---|---|---|
| Throughput | about 1,800 objects/s | about 3,800 objects/s |
| Average latency | 11 ms | 5 ms |
| p99 latency | 30 ms | 16 ms |

The gain grows with the cost of a flush on the drive: one flush serves every write that arrived while the previous
one ran.

## Enabling it

```bash
NEXO_METADATA_LOG=true nexo server /data          # or: nexo server --metadata-log /data
```

At start, each local drive converts its `xl.meta` files into the log once (the log shows `drive converted to the
metadata log`). After that the drive keeps using the log while the option is set.

**Disabling it is reversible**: start the server without the option and each drive converts the log back into
`xl.meta` files before serving. Do that before a downgrade to a version without the metadata log, which would not
see the objects of a drive in log mode.

In a cluster, set the option on every node.

## Settings

The **storage** subsystem of the configuration (console **Configuration**, or
`nexoctl admin config set <alias> storage …`) is applied at once:

| Key | Default | Meaning |
|-----|---------|---------|
| `metadata_log_segment_mib` | `64` | Size of the next segments, in MiB (1–1024). Larger segments mean fewer files, smaller ones a cheaper compaction |
| `metadata_log_memory_percent` | `25` | Share of the RAM the index of a drive may use; beyond it the server logs a warning |

The index takes about 70 bytes plus the length of the key per object (about 130 MB for a million objects with
60-byte keys).

## How it works

- **Segments**: `.nexo.sys/metalog/seg-*.log` on each drive. Every record carries a CRC-32C checksum; at start the
  segments are replayed to rebuild the index.
- **Group commit**: one writer per drive writes the records waiting at once and flushes them together; a write is
  acknowledged only once it is on stable storage, as with the files.
- **Compaction**: overwrites and deletes leave garbage. Every 30 seconds the segments with less than half of their
  bytes still live are rewritten, then removed.
- **Erasure coding and healing** work as before: each drive of a set keeps its own log, and a drive replaced or
  emptied while the server runs starts an empty log that the healing fills.
- The system volume (IAM, configuration, multipart uploads) keeps its files.

## Durability

An acknowledged write survives a restart, a crash of the process and a power loss:

- A write is acknowledged only after its record is flushed (`fdatasync`); for a large object the data directory,
  and every directory created for it, is flushed first.
- At start the segments are replayed: a record torn by a crash was never acknowledged and is cut off.
- A damaged record (a bad sector) loses only itself: the replay resumes at the next valid record and logs
  `damaged area skipped`. With erasure coding the healing restores the object from the other drives.
- The compaction copies the live records, flushed, before removing a segment, and keeps the deletes that hide
  older records until no older segment can come back.
- The conversions in either direction are written in full and flushed before the old form is removed: an
  interrupted conversion is redone at the next start.

The design is in [docs/proposals/small-object-log.md](https://github.com/R3n3r0/Nexo/blob/main/docs/proposals/small-object-log.md).
