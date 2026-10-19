# Proposal: small-object log (segmented, group commit, in-memory index)

Status: **accepted**; phase 1 in progress. Decisions: 64 MiB segments configurable from the console, the log holds the
`xl.meta` of every object (large ones too), the index memory limit is a fraction of the RAM.

## 1. Summary

Each object today has its own directory with an `xl.meta` file. Objects up to 128 KiB keep their data inline in
that file. A PUT of a small object therefore:

- creates a directory and a file;
- flushes the file (`fsync`);
- renames it into place;
- flushes the directory (a second `fsync`).

On a drive with a slow flush (consumer SSDs, HDDs, virtual disks, Windows with `FlushFileBuffers`), these two
flushes per object cap the throughput. The CPU spent creating files and taking directory locks caps it on fast
drives. Measured on the reference VM: about 1800-1900 PUT/s of 4 KiB at concurrency 20, while the request handling
alone (HTTP, SigV4, hashing) would allow several times more. A user reports about 800 PUT/s, 29 ms on average, on
a desktop disk.

This proposal stores the metadata documents of small objects in an **append-only, segmented log** per drive:

- many concurrent writes share **one flush** (group commit);
- a **per-bucket index kept in memory**, loaded from a checkpoint when the drive opens, locates each document;
- deletes and overwrites leave garbage that a **compaction** reclaims segment by segment.

The change sits entirely inside the local drive (`internal/service/storage`, `xlStorage`). The object layer,
erasure coding, healing, the scanner and the S3 API keep calling the same `StorageAPI`. Only where an `xl.meta`
document lives changes.

## 2. Goals and non-goals

Goals:

- PUT, DELETE and metadata updates of small objects bounded by CPU, not by one flush per object: target 5-10x
  on flush-bound drives, 1.5-2x on fast ones.
- Same durability as today: a write is acknowledged only once it is on stable storage.
- Same semantics: versions, delete markers, Object Lock, tags, conditional writes, listings in key order.
- GET/HEAD/listing of small objects served from the index and one read, without directory walks.
- Crash-safe: a torn tail record is detected (CRC) and discarded at replay.
- Opt-in at first (per deployment, recorded in `format.json`), default once proven.

Non-goals:

- Large objects: their data stays in data directories (files streamed, `sendfile`, O_DIRECT experiments
  unchanged). Only their `xl.meta` may move into the log in a later phase.
- A general-purpose database. The log stores opaque `xl.meta` documents keyed by (volume, path).

## 3. Design

### 3.1 Segments

```
<drive>/.nexo.sys/slog/
  seg-000000000001.log      # append-only, at most 256 MiB (configurable)
  seg-000000000002.log
  index/<volume>.ckpt       # checkpoint of the index of a volume (bucket)
  MANIFEST                  # active segment, checkpoint sequence, format version
```

Each record carries:

- a header: magic, record length, CRC-32C, a monotonic sequence number and a type (PUT, DELETE);
- the volume and the path;
- the encoded `xl.meta` (the same bytes the file holds today, including inline data).

A PUT record replaces the whole document of an object (all its versions), which keeps replay trivial. Metadata
updates (tags, retention) are PUT records too.

### 3.2 Group commit

One writer goroutine per drive owns the active segment:

1. Writers hand it a record and wait.
2. It appends every record waiting at that moment with one `writev`, flushes once (`fdatasync`, or
   `FlushFileBuffers` on Windows), then wakes all of them.
3. While a flush runs, new records queue up for the next round.

With 20 concurrent clients a round typically carries 10-20 records, so the cost of a flush is shared. A write
that arrives alone still pays one flush, about the same as today's best case, without the directory flush.

### 3.3 In-memory index

Per volume: an ordered map from path to (segment, offset, length, sequence). It is a B-tree, so that `WalkDir`
can list in key order and prefixes can be ranged.

- Memory is about 40 bytes plus the key per object: 10 million objects with 60-byte keys take about 1 GB. A
  per-drive limit (`slog.max_index_memory`) stops new objects from entering the log once reached; they fall back
  to files, which still work.
- Index segmentation (an LSM-like idea): beyond a threshold the index of a volume is split by key range into
  pages stored in the checkpoint and loaded on demand. This comes in phase 3, after the simple version is measured.
- Startup: load each volume's checkpoint, then replay the segments written after its sequence. Checkpoints are
  written in the background every N records or minutes, so a replay stays short.
- A volume is indexed lazily, on first access, so many cold buckets cost nothing until used.

### 3.4 Namespace with files and log together

An object lives either in the log or in its directory, never both:

- A new small object goes to the log.
- An object that gets a large version (data directory) is **promoted**: its `xl.meta` is written as a file,
  then a DELETE record removes it from the log.
- `ReadVersion`, `WalkDir` and the placement checks ("a parent of this key is an object", "this key is a
  prefix") consult the index first, then the file tree. `WalkDir` merges both in key order (a heap of two
  ordered streams, like the pool merge).
- Existing data stays as files. A background migration may move small objects into the log; it is optional.

### 3.5 Deletes and compaction

A DELETE record (the last version removed) drops the key from the index. Bytes of overwritten or deleted records
are counted per segment as garbage. When a non-active segment holds more than 50% garbage, the compactor copies
its live records (still the latest per the index) into the active segment, through the same group commit, then
removes the old segment. It is throttled like the scanner and pauses while the API is busy.

### 3.6 Erasure, healing, scanner

Every drive of a set runs its own log. Small objects are already inline in each drive's `xl.meta`, so erasure is
unaffected: quorum reads and writes call `ReadVersion` and `RenameData` per drive as today.

- Healing (`HealObject`, `HealDisk`) reads and writes through the same API. A drive replaced empty rebuilds its log
  through heal.
- The scanner and usage walk `WalkDir` as before.

### 3.7 Formats and compatibility

- `format.json` gets a feature flag (`slog: 1`) when the log is first enabled. Older binaries refuse the drive
  instead of missing objects, which matters for the upgrade and downgrade tests.
- `nexoctl admin` gains `slog status`, `slog compact` and `slog checkpoint`.
- Backups at file level no longer see one file per small object. The documented backup paths (S3 mirror, cluster
  export) are unaffected.

## 4. Alternatives considered

- **Index in memory over the current files.** It speeds up HEAD, GET and listing and removes some `stat` calls,
  but each PUT still creates a file and flushes twice. It is the user's starting idea; the log builds on it.
- **Batching flushes with `syncfs(2)` over the current files (Linux only).** Writers write and rename without
  flushing, and a round flushes the whole file system once. It is cheap to implement, but:
  - it needs two rounds per write to stay safe on overwrites;
  - it flushes unrelated dirty data of the same file system;
  - it has no Windows equivalent.
  Measured on the reference VM: no gain, because flushes are cheap there and the CPU is the limit. It may still
  help on flush-bound disks, so it is kept as a fallback experiment.
- **An embedded key-value store (Pebble, Badger, bbolt).** It is proven, but it duplicates our durability and
  healing model, adds a large dependency and its own compaction behaviour, and makes the on-disk format opaque to
  our tests. A purpose-built log of opaque documents is small: replay, index and compaction come to about 1500
  lines.

## 5. Risks

- **Correctness of a second namespace.** Prefix and object conflicts, listings and versions must match the
  file-based layout exactly. Mitigation: run the existing equivalence suite of the object layer and the s3-tests on
  both layouts, plus a randomized model test comparing the two.
- **Crash safety.** Mitigation: kill -9 tests during write and compaction, replay fuzzing over truncated and
  corrupted tails, and the existing multi-process fault experiments.
- **Memory.** Mitigation: the per-drive limit with fallback to files, and lazy volume loading. Index paging comes
  in phase 3.
- **Compaction I/O.** Mitigation: throttling, pausing while the API is busy, and a garbage threshold.

## 6. Testing

- Unit: record encoding, CRC, replay of torn tails, index ordering, compaction preserving the latest records.
- Storage suite (`internal/service/storage`) parameterized over both layouts, plus crash tests.
- Object layer equivalence suite, integration matrix and s3-tests on SNSD and SNMD with the log enabled.
- Benchmarks: warp PUT/GET/DELETE at concurrency 1, 4, 20 and 64 on the reference VM and on a flush-bound disk (a
  `dm-delay` device, or a loop file with `sync` mount). Linux and Windows.
- Upgrade and downgrade tests with the feature flag.

## 7. Plan

| Phase | Content |
|-------|---------|
| 1 | Log segments, group commit, index in memory, checkpoint and replay; small objects of new writes into the log behind `NEXO_SMALL_OBJECT_LOG=on`; ReadVersion/RenameData/WriteMetadata/DeleteVersion/WalkDir merge; crash tests; benchmarks |
| 2 | Compaction, promotion to files, admin commands, metrics (records per flush, flush latency, garbage) |
| 3 | Index pages by key range for very large buckets; optional background migration of existing small objects; default on after a release with it opt-in |

## 8. Decisions

1. Segments of **64 MiB** by default, changed at runtime from the console (configuration subsystem `storage`,
   `metadata_log_segment_mib`); a new size applies to the next segment.
2. The log holds the `xl.meta` of **every** object of the user volumes, large ones included: their data stays in
   data directories, but no object needs an `xl.meta` file nor, when it is small, a directory. There is no
   promotion: §3.4 simplifies to "the metadata of a drive in log mode is entirely in the log". When the log is
   enabled the drive converts its existing `xl.meta` files once, at open; when it is disabled again it converts
   the log back to files at open, so that the switch is reversible (and a downgrade possible after disabling it).
   The system volume (`.nexo.sys`: IAM, configuration, multipart uploads) keeps its files.
3. The index memory limit is a **fraction of the RAM** (`metadata_log_memory_percent`, default 25%). Phase 1
   reports the index size against it (log warning, statistics); the index pages of phase 3 enforce it.
