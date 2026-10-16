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

## 7. Open work

- O_DIRECT for large shards and zero-copy writes (`io.ReaderFrom`/`sendfile`) from the drives to the socket: not
  measurable on tmpfs, to be evaluated on NVMe.
- Multi-buffer MD5 (AVX-512) for the ETag: 18% of the CPU of a large PUT.
- A pool of Decode buffers for whole blocks (today reallocated per request: the in-flight reads of slow shards keep
  owning them, reference counting is needed).
- Benchmarks on separate nodes with a real network (TP-01..TP-04) and memory stability tests (TP-06).
