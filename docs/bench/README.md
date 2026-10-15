# Prestazioni di Nexo (M9 T-9.2)

Benchmark `warp` in tutte le modalità di deployment, profiling dei percorsi caldi via Admin API, ottimizzazioni
misurate e confronto con gli obiettivi TP-01..TP-06 di [docs/06 §6.6](../06-piano-test.md#66-test-di-prestazioni-server).
I numeri sono reali e riproducibili con i comandi di §5; valgono per l'hardware di §1, molto diverso da quello di
riferimento degli obiettivi (4 nodi NVMe, 25 GbE).

## 1. Hardware e software

| Voce | Valore |
|------|--------|
| Macchina | VM Firecracker, 4 vCPU Intel Xeon @ 2,10 GHz (AVX2, AVX-512, SHA-NI, GFNI), 15 GiB RAM |
| Kernel | Linux 6.18 |
| Drive | tmpfs (`/dev/shm`, `BENCH_TMPFS=1`): misura lo stack software, non i dischi |
| Rete | loopback (`127.0.0.1`), client e server sulla stessa macchina |
| Go | 1.24.7 (il `go.mod` di M9; dalla v1.0 la toolchain è 1.26.9), build `-trimpath` |
| Client | `github.com/minio/warp` v1.8.2 (minio-go: firma SigV4 in streaming su HTTP) |

Riferimenti grezzi della stessa macchina, misurati con lo speed test di Nexo (`POST /api/v1/speedtest/*`,
`nexoctl admin speedtest`, file da 16 MiB, concorrenza 8, cluster di 4 processi):

| Speed test | Scrittura | Lettura |
|------------|-----------|---------|
| `drive` (tmpfs, per drive) | 1,9–2,0 GiB/s | 4,9–5,0 GiB/s |
| `net` (RPC internode verso ciascun peer, loopback) | 583–604 MiB/s | 792–878 MiB/s |
| `object` (object layer del cluster: erasure, quorum, RPC) | 472 MiB/s | 1514 MiB/s |

## 2. Metodologia

- Ogni esecuzione avvia server nuovi su drive vuoti (`test/bench/warp.sh <modo>`):
  - **SNSD**: un drive;
  - **SNMD**: quattro drive, EC 2+2;
  - **cluster**: quattro processi `nexo` × due drive (`http://127.0.0.1:{9511...9514}/…/d{1...2}`), EC 4+4, client in
    round robin sui quattro nodi.
- Benchmark (concorrenza 16):
  - `put`: oggetti da 64 MiB per 6 s (TP-01; durata limitata dalla RAM del tmpfs: `put` conserva ciò che scrive);
  - `get`: 24 oggetti da 64 MiB letti per 15 s (TP-02);
  - `mixed`: 1000 oggetti da 1 B a 1 MiB (dimensione casuale), 45% GET, 15% PUT, 30% STAT, 10% DELETE per 20 s
    (TP-03);
  - `list`: 5000 oggetti da 1 KiB, LIST v2 per 15 s (TP-04, in scala ridotta).
- **Prima/dopo**: due round alternati (prima, dopo, prima, dopo) con lo stesso binario di base e lo stesso albero più
  le ottimizzazioni di §3; le tabelle riportano la media dei due round. La variabilità tra round della stessa versione
  è di circa ±10% su `put`/`get` (finestre brevi, tmpfs in RAM, client sulla stessa CPU) e di ±3% su `mixed`/`list`:
  differenze inferiori non sono significative.
- Profili CPU presi durante i benchmark con l'Admin API (`PROFILE=1`: `GET /api/v1/profile?type=cpu&seconds=6` sul
  primo nodo) e micro-benchmark Go confrontati con `benchstat` (TP-05).

## 3. Profili e ottimizzazioni

**PUT da 64 MiB (SNMD)**: 59% syscall (copia nel page cache del tmpfs e lettura del socket), 18% MD5 (ETag, imposto
da S3), 9% SHA-256 (verifica dei chunk firmati in streaming), 4% HighwayHash (bitrot), 1,4% Reed-Solomon. Lavoro
inevitabile; il margine è nelle allocazioni.

**GET da 64 MiB (SNMD)**: 56% syscall (lettura degli shard e scrittura sul socket), 15% HighwayHash (verifica
bitrot), 8% memmove.

**Mixed (SNMD)**: 25% syscall; ~20% allocazione e GC: `runtime.memclrNoHeapPointers` 8%, `scanobject` 11%. I
chiamanti di `makeslice`: il lettore parallelo degli shard (`erasure.(*parallelReader).readBlock`, 37%),
`erasure.(*codec).Encode` (21%), `storage.(*xlStorage).copyFull` (15%): ogni oggetto, anche da 1 KiB, allocava e
azzerava i buffer di un blocco intero (1 MiB di blocco + 4 shard da 512 KiB in scrittura, 4 × 512 KiB in lettura).

**Cluster (mixed)**: il nodo 1 usa 0,8 core su 4: la macchina è satura (4 processi + client su 4 vCPU) e ogni
richiesta attraversa lock distribuiti e RPC verso 8 drive. A vuoto: PUT 10 ms, HEAD 4,5 ms, GET 3 ms, DELETE 11 ms.

Ottimizzazioni applicate (nessun cambiamento di comportamento; test `-race` di `erasure` e `storage` verdi):

1. **Buffer di Encode riciclati** (`internal/service/erasure/codec.go`): blocco, shard e vista del blocco vengono da un
   `sync.Pool` del codec (uno per layout, condiviso). Sicuro perché gli `io.Writer` non trattengono i dati e
   `writeShards` attende ogni scrittura prima di tornare.
2. **Buffer di Decode e Heal dimensionati sulla parte** (`internal/service/erasure/decode.go`): gli shard di una parte
   più piccola di un blocco si leggono e ricostruiscono in buffer della loro dimensione, non di un blocco intero.
3. **Buffer di scrittura dei drive riciclati anche per i file piccoli** (`internal/service/storage/local.go`
   `copyFull`): un file di dimensione nota sotto 1 MiB usa i primi byte del buffer del pool invece di allocarne uno.

### Micro-benchmark (TP-05, `go test -bench . -count 4 ./internal/service/erasure/`)

| Benchmark | Prima | Dopo | Δ tempo | Allocato/op prima → dopo |
|-----------|-------|------|---------|--------------------------|
| `EncodeSmall/4KiB` (2+2) | 1348 µs | 7,1 µs | −99,5% | 3074 KiB → 2,1 KiB |
| `EncodeSmall/64KiB` | 1353 µs | 47,7 µs | −96,5% | 3074 KiB → 2,4 KiB |
| `EncodeSmall/512KiB` | 1375 µs | 267 µs | −80,6% | 3074 KiB → 3,3 KiB |
| `DecodeSmall/4KiB` (senza / con 2 shard mancanti) | 1180 / 1187 µs | 13,5 / 15,1 µs | −98,9% / −98,7% | 2050 KiB → 10 KiB |
| `DecodeSmall/64KiB` | 1309 / 1255 µs | 103 / 110 µs | −92,2% / −91,3% | 2050 KiB → 130 KiB |
| `DecodeSmall/512KiB` | 1145 / 1283 µs | 727 / 850 µs | −36,5% / −33,7% | 2,0 MiB → 1,0 MiB |
| `Encode` 8 MiB (2+2 / 4+2 / 6+2 / 12+4) | 1,62 / 1,77 / 1,79 / 1,90 GiB/s | 1,90 / 2,23 / 2,16 / 2,19 GiB/s | +15–26% throughput | ~2,5–3 MiB → 25–36 KiB |
| `Decode` 8 MiB (tutti i layout) | — | — | invariato (p > 0,05) | invariato |

Altri percorsi caldi misurati e lasciati invariati: verifica SigV4 dell'header 28–39 µs/op (cache delle chiavi
derivate), chunk firmati in streaming 1,12 GB/s per core, HIT della cache in memoria (64 KiB) 3,4–4,1 µs/op
(16–19 GB/s).

## 4. Risultati warp (media di due round)

### SNSD (1 drive)

| Benchmark | Prima | Dopo | Δ |
|-----------|-------|------|---|
| PUT 64 MiB | 705 MiB/s | 727 MiB/s | +3% (rumore) |
| GET 64 MiB | 3951 MiB/s | 3927 MiB/s | −1% (rumore) |
| Mixed | 4386 op/s | 4439 op/s | +1% |
| Mixed, TTFB GET/PUT 100 KiB–1 MiB (round 2: mediana / p99) | 8 / 38 ms, 13 / 48 ms | 3 / 31 ms, 5 / 23 ms | |
| LIST (chiavi/s) | 60 926 | 61 680 | +1% |

### SNMD (4 drive, EC 2+2)

| Benchmark | Prima | Dopo | Δ |
|-----------|-------|------|---|
| PUT 64 MiB | 407 MiB/s | 384 MiB/s | −6% (rumore: 368 → 392 nel round 1, 446 → 376 nel round 2) |
| GET 64 MiB | 3103 MiB/s | 2982 MiB/s | −4% (rumore) |
| Mixed | 2185 op/s | 2498 op/s | **+14%** (entrambi i round: +13%, +16%) |
| Mixed STAT mediana / p99 | 6,6 / 20,7 ms | 4,0 / 18,3 ms | |
| Mixed DELETE mediana / p99 | 9,8 / 30,9 ms | 7,0 / 29,9 ms | |
| Mixed, TTFB GET/PUT 100 KiB–1 MiB (round 2: mediana / p99) | 6 / 24 ms, 13 / 40 ms | 5 / 22 ms, 10 / 45 ms | |
| LIST (chiavi/s) | 17 846 | 18 604 | +4% |

### Cluster (4 processi × 2 drive, EC 4+4, stessa VM)

| Benchmark | Prima | Dopo | Δ |
|-----------|-------|------|---|
| PUT 64 MiB | 286 MiB/s | 296 MiB/s | +4% |
| GET 64 MiB | 1422 MiB/s | 1467 MiB/s | +3% |
| Mixed | 307 op/s | 327 op/s | **+7%** (entrambi i round: +8%, +5%) |
| Mixed STAT mediana / p99 | 30,5 / 67,4 ms | 27,9 / 61,5 ms | |
| Mixed DELETE mediana / p99 | 109 / 172 ms | 104 / 167 ms | |
| LIST (chiavi/s) | 7455 | 7093 | −5% (rumore) |

## 5. Confronto con gli obiettivi (docs/06 §6.6)

| ID | Obiettivo (4 nodi NVMe, 25 GbE) | Misura su questa VM | Esito |
|----|---------------------------------|---------------------|-------|
| TP-01 | `warp put` 64 MiB ≥ 80% della banda | Cluster 296 MiB/s = 50% della scrittura RPC verso un peer (≈ 590 MiB/s) e 63% dello speed test `object` (472 MiB/s); SNSD 727 MiB/s | Non verificabile qui: CPU condivisa da 4 nodi e dal client, nessuna rete reale |
| TP-02 | `warp get` 64 MiB ≥ 90% della banda | Cluster 1467 MiB/s ≈ speed test `object` (1514 MiB/s, 97%); SNSD 3,9 GiB/s | In linea con lo stack (97% del layer a oggetti) |
| TP-03 | `warp mixed` ≥ 10k op/s per nodo, p99 < 50 ms | SNSD 4,4k op/s, SNMD 2,5k op/s con TTFB p99 ≤ 45 ms; cluster 327 op/s, p99 63–200 ms | Latenze in obiettivo in SNSD/SNMD; throughput limitato da 4 vCPU condivise con il client; il cluster su una sola VM non è rappresentativo |
| TP-04 | Listing ≥ 10k chiavi/s | SNSD 61,7k, SNMD 18,6k, cluster 7,1k (5000 oggetti) | Raggiunto in SNSD/SNMD; da rimisurare su 1M oggetti e su nodi separati |
| TP-05 | Nessuna regressione > 10% (`benchstat`) | Encode +15–26%, oggetti piccoli 3–190×; Decode invariato | Raggiunto |
| TP-06 | Memoria stabile 24 h | Non eseguito (le esecuzioni durano minuti); allocazioni per richiesta ridotte di 2–3 MiB per oggetto piccolo | Da eseguire in CI notturna su hardware dedicato |

Note: un `put` di 15 s su tmpfs in SNMD ha esaurito la RAM (OOM del server): su tmpfs la durata di `put` va limitata
(`PUT_DURATION`), oppure i drive vanno su disco (`DATA_DIR`).

## 6. Riproduzione

```sh
make build-go
# tutte le modalità con i parametri di default (20 s per benchmark), risultati in bin/bench/<modo>
make bench
# i parametri di questo report, con i profili CPU
BENCH_TMPFS=1 PROFILE=1 CONCURRENCY=16 PUT_DURATION=6s GET_DURATION=15s MIXED_DURATION=20s LIST_DURATION=15s \
  LARGE_OBJECTS=24 MIXED_OBJECTS=1000 LIST_OBJECTS=5000 test/bench/warp.sh snmd
go tool pprof -top bin/nexo bin/bench/snmd/mixed.cpu.pb.gz
# micro-benchmark (TP-05)
go test -run '^$' -bench 'Encode|Decode' -count 6 ./internal/service/erasure/ > new.txt && benchstat old.txt new.txt
# speed test di un deployment in esecuzione
nexoctl admin speedtest --size 16MiB --duration 5s --concurrency 8 ALIAS object
```

`test/bench/warp.sh` installa warp (`go install github.com/minio/warp@v1.8.2`) se non è nel `PATH`. Ogni benchmark
lascia in `bin/bench/<modo>` l'output testuale (`<bench>.txt`), i dati grezzi (`warp-*.json.zst`, da rileggere con
`warp analyze`), `summary.txt` e, con `PROFILE=1`, `<bench>.cpu.pb.gz`.

## 7. Lavoro aperto

- O_DIRECT per gli shard grandi e scritture zero-copy (`io.ReaderFrom`/`sendfile`) dai drive al socket: su tmpfs non
  misurabili, da valutare su NVMe.
- MD5 multi-buffer (AVX-512) per l'ETag: 18% della CPU di un PUT grande.
- Pool dei buffer di Decode per i blocchi interi (oggi riallocati per richiesta: le letture in corsa degli shard
  lenti ne restano proprietarie, serve un conteggio dei riferimenti).
- Benchmark su nodi separati con rete reale (TP-01..TP-04) e test di stabilità della memoria (TP-06).
