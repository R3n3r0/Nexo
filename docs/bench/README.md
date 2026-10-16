# Nexo performance (M9 T-9.2)

`warp` benchmarks in every deployment mode, profiling of the hot paths through the Admin API, measured optimizations
and a comparison with the TP-01..TP-06 goals of [docs/06 §6.6](../06-test-plan.md#66-server-performance-tests).
The numbers are real and reproducible with the commands in §6; they apply to the hardware in §1, very different from
the reference hardware of the goals (4 NVMe nodes, 25 GbE).

## 1. Hardware and software

| Item | Value |
|------|--------|
| Machine | Firecracker VM, 4 vCPU Intel Xeon @ 2.10 GHz (AVX2, AVX-512, SHA-NI, GFNI), 15 GiB RAM |
| Kernel | Linux 6.18 |
| Drives | tmpfs (`/dev/shm`, `BENCH_TMPFS=1`): measures the software stack, not the disks |
| Network | loopback (`127.0.0.1`), client and server on the same machine |
| Go | 1.24.7 (the `go.mod` of M9; since v1.0 the toolchain is 1.26.9), `-trimpath` build |
| Client | `github.com/minio/warp` v1.8.2 (minio-go: streaming SigV4 signing over HTTP) |

Raw baselines of the same machine, measured with Nexo's speed test (`POST /api/v1/speedtest/*`,
`nexoctl admin speedtest`, 16 MiB files, concurrency 8, cluster of 4 processes):

| Speed test | Write | Read |
|------------|-----------|---------|
| `drive` (tmpfs, per drive) | 1.9–2.0 GiB/s | 4.9–5.0 GiB/s |
| `net` (internode RPC to each peer, loopback) | 583–604 MiB/s | 792–878 MiB/s |
| `object` (cluster object layer: erasure, quorum, RPC) | 472 MiB/s | 1514 MiB/s |

## 2. Methodology

- Every run starts fresh servers on empty drives (`test/bench/warp.sh <mode>`):
  - **SNSD**: one drive;
  - **SNMD**: four drives, EC 2+2;
  - **cluster**: four `nexo` processes × two drives (`http://127.0.0.1:{9511...9514}/…/d{1...2}`), EC 4+4, client
    round robin over the four nodes.
- Benchmarks (concurrency 16):
  - `put`: 64 MiB objects for 6 s (TP-01; duration limited by the tmpfs RAM: `put` keeps what it writes);
  - `get`: 24 objects of 64 MiB read for 15 s (TP-02);
  - `mixed`: 1000 objects from 1 B to 1 MiB (random size), 45% GET, 15% PUT, 30% STAT, 10% DELETE for 20 s
    (TP-03);
  - `list`: 5000 objects of 1 KiB, LIST v2 for 15 s (TP-04, at a reduced scale).
- **Before/after**: two alternating rounds (before, after, before, after) with the same base binary and the same tree
  plus the optimizations of §3; the tables report the mean of the two rounds. The variability between rounds of the
  same version is about ±10% on `put`/`get` (short windows, tmpfs in RAM, client on the same CPU) and ±3% on
  `mixed`/`list`: smaller differences are not significant.
- CPU profiles taken during the benchmarks with the Admin API (`PROFILE=1`: `POST /api/v1/profile?type=cpu&seconds=6`
  on the first node) and Go micro-benchmarks compared with `benchstat` (TP-05).

## 3. Profiles and optimizations

**64 MiB PUT (SNMD)**: 59% syscalls (copy into the tmpfs page cache and socket reads), 18% MD5 (ETag, mandated by
S3), 9% SHA-256 (verification of the streaming signed chunks), 4% HighwayHash (bitrot), 1.4% Reed-Solomon.
Unavoidable work; the margin is in the allocations.

**64 MiB GET (SNMD)**: 56% syscalls (reading the shards and writing to the socket), 15% HighwayHash (bitrot
verification), 8% memmove.

**Mixed (SNMD)**: 25% syscalls; ~20% allocation and GC: `runtime.memclrNoHeapPointers` 8%, `scanobject` 11%. The
callers of `makeslice`: the parallel shard reader (`erasure.(*parallelReader).readBlock`, 37%),
`erasure.(*codec).Encode` (21%), `storage.(*xlStorage).copyFull` (15%): every object, even a 1 KiB one, allocated
and zeroed the buffers of a whole block (1 MiB block + 4 shards of 512 KiB on write, 4 × 512 KiB on read).

**Cluster (mixed)**: node 1 uses 0.8 cores out of 4: the machine is saturated (4 processes + client on 4 vCPUs) and
every request goes through distributed locks and RPCs to 8 drives. Unloaded: PUT 10 ms, HEAD 4.5 ms, GET 3 ms,
DELETE 11 ms.

Optimizations applied (no behavior change; `-race` tests of `erasure` and `storage` green):

1. **Recycled Encode buffers** (`internal/service/erasure/codec.go`): block, shards and block view come from a
   `sync.Pool` of the codec (one per layout, shared). Safe because the `io.Writer`s do not retain the data and
   `writeShards` waits for every write before returning.
2. **Decode and Heal buffers sized on the part** (`internal/service/erasure/decode.go`): the shards of a part smaller
   than a block are read and reconstructed in buffers of their size, not of a whole block.
3. **Drive write buffers recycled for small files too** (`internal/service/storage/local.go` `copyFull`): a file of
   known size under 1 MiB uses the first bytes of the pool buffer instead of allocating one.

### Micro-benchmarks (TP-05, `go test -bench . -count 4 ./internal/service/erasure/`)

| Benchmark | Before | After | Δ time | Allocated/op before → after |
|-----------|-------|------|---------|--------------------------|
| `EncodeSmall/4KiB` (2+2) | 1348 µs | 7.1 µs | −99.5% | 3074 KiB → 2.1 KiB |
| `EncodeSmall/64KiB` | 1353 µs | 47.7 µs | −96.5% | 3074 KiB → 2.4 KiB |
| `EncodeSmall/512KiB` | 1375 µs | 267 µs | −80.6% | 3074 KiB → 3.3 KiB |
| `DecodeSmall/4KiB` (without / with 2 missing shards) | 1180 / 1187 µs | 13.5 / 15.1 µs | −98.9% / −98.7% | 2050 KiB → 10 KiB |
| `DecodeSmall/64KiB` | 1309 / 1255 µs | 103 / 110 µs | −92.2% / −91.3% | 2050 KiB → 130 KiB |
| `DecodeSmall/512KiB` | 1145 / 1283 µs | 727 / 850 µs | −36.5% / −33.7% | 2.0 MiB → 1.0 MiB |
| `Encode` 8 MiB (2+2 / 4+2 / 6+2 / 12+4) | 1.62 / 1.77 / 1.79 / 1.90 GiB/s | 1.90 / 2.23 / 2.16 / 2.19 GiB/s | +15–26% throughput | ~2.5–3 MiB → 25–36 KiB |
| `Decode` 8 MiB (all layouts) | — | — | unchanged (p > 0.05) | unchanged |

Other hot paths measured and left unchanged: header SigV4 verification 28–39 µs/op (cache of derived keys),
streaming signed chunks 1.12 GB/s per core, in-memory cache HIT (64 KiB) 3.4–4.1 µs/op (16–19 GB/s).

## 4. warp results (mean of two rounds)

### SNSD (1 drive)

| Benchmark | Before | After | Δ |
|-----------|-------|------|---|
| PUT 64 MiB | 705 MiB/s | 727 MiB/s | +3% (noise) |
| GET 64 MiB | 3951 MiB/s | 3927 MiB/s | −1% (noise) |
| Mixed | 4386 op/s | 4439 op/s | +1% |
| Mixed, TTFB GET/PUT 100 KiB–1 MiB (round 2: median / p99) | 8 / 38 ms, 13 / 48 ms | 3 / 31 ms, 5 / 23 ms | |
| LIST (keys/s) | 60 926 | 61 680 | +1% |

### SNMD (4 drives, EC 2+2)

| Benchmark | Before | After | Δ |
|-----------|-------|------|---|
| PUT 64 MiB | 407 MiB/s | 384 MiB/s | −6% (noise: 368 → 392 in round 1, 446 → 376 in round 2) |
| GET 64 MiB | 3103 MiB/s | 2982 MiB/s | −4% (noise) |
| Mixed | 2185 op/s | 2498 op/s | **+14%** (both rounds: +13%, +16%) |
| Mixed STAT median / p99 | 6.6 / 20.7 ms | 4.0 / 18.3 ms | |
| Mixed DELETE median / p99 | 9.8 / 30.9 ms | 7.0 / 29.9 ms | |
| Mixed, TTFB GET/PUT 100 KiB–1 MiB (round 2: median / p99) | 6 / 24 ms, 13 / 40 ms | 5 / 22 ms, 10 / 45 ms | |
| LIST (keys/s) | 17 846 | 18 604 | +4% |

### Cluster (4 processes × 2 drives, EC 4+4, same VM)

| Benchmark | Before | After | Δ |
|-----------|-------|------|---|
| PUT 64 MiB | 286 MiB/s | 296 MiB/s | +4% |
| GET 64 MiB | 1422 MiB/s | 1467 MiB/s | +3% |
| Mixed | 307 op/s | 327 op/s | **+7%** (both rounds: +8%, +5%) |
| Mixed STAT median / p99 | 30.5 / 67.4 ms | 27.9 / 61.5 ms | |
| Mixed DELETE median / p99 | 109 / 172 ms | 104 / 167 ms | |
| LIST (keys/s) | 7455 | 7093 | −5% (noise) |

## 5. Comparison with the goals (docs/06 §6.6)

| ID | Goal (4 NVMe nodes, 25 GbE) | Measured on this VM | Outcome |
|----|---------------------------------|---------------------|-------|
| TP-01 | `warp put` 64 MiB ≥ 80% of the bandwidth | Cluster 296 MiB/s = 50% of the RPC write to one peer (≈ 590 MiB/s) and 63% of the `object` speed test (472 MiB/s); SNSD 727 MiB/s | Not verifiable here: CPU shared by 4 nodes and the client, no real network |
| TP-02 | `warp get` 64 MiB ≥ 90% of the bandwidth | Cluster 1467 MiB/s ≈ `object` speed test (1514 MiB/s, 97%); SNSD 3.9 GiB/s | In line with the stack (97% of the object layer) |
| TP-03 | `warp mixed` ≥ 10k op/s per node, p99 < 50 ms | SNSD 4.4k op/s, SNMD 2.5k op/s with TTFB p99 ≤ 45 ms; cluster 327 op/s, p99 63–200 ms | Latencies on target in SNSD/SNMD; throughput limited by 4 vCPUs shared with the client; the cluster on a single VM is not representative |
| TP-04 | Listing ≥ 10k keys/s | SNSD 61.7k, SNMD 18.6k, cluster 7.1k (5000 objects) | Met in SNSD/SNMD; to be remeasured on 1M objects and on separate nodes |
| TP-05 | No regression > 10% (`benchstat`) | Encode +15–26%, small objects 3–190×; Decode unchanged | Met |
| TP-06 | Stable memory over 24 h | Not run (the runs last minutes); per-request allocations reduced by 2–3 MiB per small object | To be run in nightly CI on dedicated hardware |

Note: a 15 s `put` on tmpfs in SNMD exhausted the RAM (server OOM): on tmpfs the duration of `put` must be limited
(`PUT_DURATION`), or the drives must be put on disk (`DATA_DIR`).

## 6. Reproduction

```sh
make build-go
# all modes with the default parameters (20 s per benchmark), results in bin/bench/<mode>
make bench
# the parameters of this report, with the CPU profiles
BENCH_TMPFS=1 PROFILE=1 CONCURRENCY=16 PUT_DURATION=6s GET_DURATION=15s MIXED_DURATION=20s LIST_DURATION=15s \
  LARGE_OBJECTS=24 MIXED_OBJECTS=1000 LIST_OBJECTS=5000 test/bench/warp.sh snmd
go tool pprof -top bin/nexo bin/bench/snmd/mixed.cpu.pb.gz
# micro-benchmarks (TP-05)
go test -run '^$' -bench 'Encode|Decode' -count 6 ./internal/service/erasure/ > new.txt && benchstat old.txt new.txt
# speed test of a running deployment
nexoctl admin speedtest --size 16MiB --duration 5s --concurrency 8 ALIAS object
```

`test/bench/warp.sh` installs warp (`go install github.com/minio/warp@v1.8.2`) if it is not in the `PATH`. Every
benchmark leaves in `bin/bench/<mode>` the text output (`<bench>.txt`), the raw data (`warp-*.json.zst`, to be reread
with `warp analyze`), `summary.txt` and, with `PROFILE=1`, `<bench>.cpu.pb.gz`.

## 7. O_DIRECT and zero-copy (T-9.2, October 2026)

Two optimizations of the data path evaluated on the same VM (§1, Go 1.27.2), measured before and after with the
same tree and a build of the base commit side by side. The machine was shared with another workload (load average
2–26 during the runs), so the throughput of single runs varies by ±30–60%: the decisions rest on repeated,
alternating runs (base and candidate in turns, the order reversed every round), on `benchstat` for the Go
benchmarks and on an exact Mann-Whitney test for the end-to-end samples, and on the CPU time per GiB of the server,
which the load of the machine barely moves.

**End-to-end harness.** Besides `test/bench/warp.sh`, whose 64 MiB `put` keeps every object and could not run long
enough on the free space of the VM disk, a small S3 load generator (minio-go, 8 clients, 64 MiB and 4 MiB objects,
12 s PUT and 10 s GET per point) overwrote one key per client, so the space used stays bounded, and read the CPU
time of the server from `/proc/<pid>/stat`. Drives on the VM disk (ext4 on virtio, `fsync` on) and on tmpfs
(`/dev/shm`); SNSD (1 drive) and SNMD (4 drives, EC 2+2).

Baseline of the base commit (first rounds, load 9–26):

| Mode, drives | PUT 64 MiB | GET 64 MiB | PUT 4 MiB | GET 4 MiB | GET CPU (s/GiB, 64 / 4 MiB) |
|--------------|-----------:|-----------:|----------:|----------:|----------------------------:|
| SNSD, disk | 291 MiB/s | 1153 MiB/s | 230 MiB/s | 873 MiB/s | 0.48 / 0.60 |
| SNMD, disk | 229 MiB/s | 2655 MiB/s | 202 MiB/s | 1858 MiB/s | 0.69 / 1.13 |
| SNSD, tmpfs | 457 MiB/s | 1141 MiB/s | 677 MiB/s | 3058 MiB/s | 0.59 / 0.57 |
| SNMD, tmpfs | 281 MiB/s | 937 MiB/s | 254 MiB/s | 619 MiB/s | 0.83 / 1.32 |

Medians of 2 (disk) and 5 (tmpfs) runs; warp on tmpfs gave the same picture (SNSD GET 1.8–2.2 GiB/s, mixed
1.7–2.6k op/s; SNMD GET 0.8–1.5 GiB/s), well below §4 because of the load of the machine.

### 7.1 Zero-copy downloads: kept

A GET of a single-drive version without encryption is the bytes of its part files as they are on the drive (no
erasure coding, no bitrot frames, no compression in the object layer). Its body now reaches the socket through
`sendfile(2)`: the kernel copies the page cache to the socket, with no `pread` into a user-space buffer and no
`write` from it.

- `storage`: the stream of `ReadFileStream` (one file descriptor per stream) implements `io.WriterTo`: a writer
  with `io.ReaderFrom` gets the rest of the section as an `*io.LimitedReader` over the `*os.File`, positioned at
  the section (the shared descriptor of a pinned `FileHandle` is never moved).
- `object`: `Reader`, the parts reader and the lock-holding wrapper forward `io.WriterTo`, part by part (ranges,
  multipart objects, pinned and lock-held reads); from the first part stream that does not write itself (erasure,
  remote drive, SSE) the rest is copied through `Read` exactly as before.
- `httpx`: `SendGuard` (the per-write stall bound of downloads) implements `io.ReaderFrom`: a file section goes
  in pieces of 256 KiB, the deadline armed before each piece; a piece whose deadline expires after at least
  32 KiB went out (one buffered write of `io.Copy`) is resumed under a new deadline, so a slow but steady client is
  held to the same rate as with plain writes and a stalled one is still cut within the send timeout. Any other
  body is copied through `Write` as before.
- The writers between the handler and `net/http` (`middleware` byte counting, the lifecycle `x-amz-expiration`
  writer, the server stall guard) forward file sections through `io.ReaderFrom`, counting them and arming their
  own deadlines per piece; anything else, and every HTTP/2 stream, goes through `Write` as before. Over TLS
  `net/http` falls back to a buffered copy by itself.

Results (load generator, server CPU per GiB sent; *n* runs per side, base vs zero-copy):

| Point | Base | Zero-copy | Δ | p |
|-------|-----:|----------:|--:|--:|
| SNSD tmpfs, GET 64 MiB, CPU | 0.59 s/GiB | 0.20 s/GiB | **−66%** | 0.003 (n=5/7) |
| SNSD tmpfs, GET 4 MiB, CPU | 0.57 s/GiB | 0.30 s/GiB | **−47%** | 0.003 (n=5/7) |
| SNSD disk, GET 64 MiB, CPU | 0.48 s/GiB | 0.19 s/GiB | −60% | 0.13 (n=2/4, every sample apart) |
| SNSD disk, GET 4 MiB, CPU | 0.60 s/GiB | 0.28 s/GiB | −53% | 0.13 (n=2/4, every sample apart) |
| SNSD tmpfs, GET 64 MiB, throughput | 1141 MiB/s | 3108 MiB/s | +172% | 0.27 (not significant) |
| SNSD tmpfs, GET 4 MiB, throughput | 3058 MiB/s | 2214 MiB/s | −28% | 0.88 (not significant) |
| SNMD, PUT (every point), CPU and throughput | | | ±4% CPU | > 0.05 (unchanged path) |

`strace -c` of a 64 MiB GET: 327 `sendfile` calls, 12 `pread64`. `BenchmarkCopyBodyFile` (`httpx`, 16 MiB
file section served over loopback HTTP/1.1 to an in-process client, 10 alternating runs): same throughput
(6.1 vs 5.8 GiB/s, p=0.22, bound by the client copying out of its socket) and 12% less CPU for the whole process,
client included (8.9 vs 10.1 ms/op, p=0.009). The micro-benchmarks of `storage` and `rpc` show no significant
change (`benchstat`, n=8, every p > 0.05 except a +112% on `CreateFile/nosync/64KiB`, a path the change does not
touch: noise).

Not applicable, left unchanged:

- **SNMD and cluster GET, SSE**: the bytes are decoded, verified (bitrot) or decrypted in user space.
- **Internode RPC** (`ReadFileStream` and file handles of remote drives): the shards travel in the data frames of
  the RPC protocol inside HTTP/2 (and TLS between nodes), which must be assembled in user space; `splice` cannot
  apply, nor `sendfile` on the upload side (an HTTP/2 request body is not a socket).
- **Distribution cache, L2 disk hits**: an entry is read whole and its CRC verified before it is served.
- **Console static assets**: embedded in the binary (`embed.FS`), not files.

### 7.2 O_DIRECT for large data files: rejected

The candidate wrote the temporary data file of `CreateFile` (SNSD parts, erasure shards, heal) with `O_DIRECT` from
its first whole 1 MiB buffer on: buffers aligned to the alignment the file system reports (`statx` `STATX_DIOALIGN`,
4 KiB by default), the unaligned tail written after clearing `O_DIRECT` with `fcntl(F_SETFL)`, the `fsync` before
the rename unchanged, a drive-wide fallback to the page cache when the file system refuses it (`EINVAL` at `fcntl`
or at the first write), only on drives that sync their writes, xl.meta and small files always buffered, and a
`NEXO_DRIVE_DIRECT_IO=off` switch. It was correct (unit tests of the alignment, tail and fallback paths, the
storage conformance suite with fsync on), but slower on this disk:

| Measurement | Page cache + fsync (base) | O_DIRECT | Δ |
|-------------|--------------------------:|---------:|--:|
| `BenchmarkCreateFile/sync/4MiB` (ext4/virtio, n=6) | 416 MiB/s | 290 MiB/s | **−30%** (p=0.002) |
| `BenchmarkCreateFile/sync/64MiB` (n=6) | 509 MiB/s | 362 MiB/s | −29% (p=0.065) |
| `BenchmarkCreateFileParallel` (4 × 4 MiB, n=6) | 440 MiB/s | 361 MiB/s | −18% (p=0.59) |
| Prototype: 64 MiB file, 1 MiB writes, then fsync | 411–425 MiB/s | 328–347 MiB/s | −20% |
| Same, 4 MiB direct writes / writes overlapped with the producer (2 buffers, writer goroutine) | | 317–341 / 357–396 MiB/s | −10 to −25% |
| Load generator, SNMD disk, PUT 4 MiB (n=2) | 202 MiB/s | 97 MiB/s | −52% |
| Load generator, SNMD disk, PUT 64 MiB | 229 MiB/s | 193 MiB/s | −16% |
| Load generator, SNSD disk, PUT 64 / 4 MiB (n=2) | 291 / 230 MiB/s | 337 / 208 MiB/s | +16% / −10% (the same binary without O_DIRECT: 394 / 365) |
| GET right after the PUT, SNMD disk 64 MiB | 2655 MiB/s (page cache) | 1193 MiB/s (from the disk) | −55% |

A direct write blocks until the device completes it, one request at a time per stream, where a buffered write
returns once copied and the final `fsync` hands the device the whole file at once; the CPU saved (no copy into the
page cache: 2.4 vs 3.3 s/GiB on a 64 MiB SNSD PUT) does not make up for it, and the data just written is no longer
in the page cache for the reads that follow. Direct reads were measured with `dd` only and not built: 1.9 GB/s
against 2.4 GB/s for a cold buffered read with read-ahead and 7.1 GB/s from the page cache (9–14 GB/s in
`BenchmarkReadFileStream`), and a GET of a hot object would always pay the device. On NVMe with deep queues the
balance may change; the evaluation patch is not in the tree, and `BenchmarkCreateFile` (`internal/service/storage`,
`NEXO_BENCH_DIR` selects the file system) is there to measure it again.

### 7.3 Reproduction

```sh
# micro-benchmarks, base and candidate trees alternated, then benchstat
NEXO_BENCH_DIR=/mnt/disk go test -run '^$' -bench . -count 2 ./internal/service/storage/ >> new.txt
go test -run '^$' -bench 'CreateFile|ReadFileStream' -benchtime 2s -count 2 ./internal/transport/rpc/ >> new-rpc.txt
go test -c -o httpx.test ./internal/transport/http/httpx/
for i in $(seq 10); do ./httpx.test -test.run '^$' -test.bench CopyBodyFile; done > copy.txt
benchstat -col /src copy.txt
# zero-copy at work: sendfile calls of a running SNSD server during a GET
strace -f -c -e trace=sendfile,pread64 -p "$(pidof nexo)"
```

## 8. Open work

- O_DIRECT (§7.2) on NVMe with deep queues, where it may pay off; zero-copy for HTTP/2 and TLS downloads (kTLS)
  is out of reach of `net/http`.
- Multi-buffer MD5 (AVX-512) for the ETag: 18% of the CPU of a large PUT.
- A pool of Decode buffers for whole blocks (today reallocated per request: the in-flight reads of slow shards keep
  owning them, reference counting is needed).
- Benchmarks on separate nodes with a real network (TP-01..TP-04) and memory stability tests (TP-06).
