# Video pipeline performance

Go benchmarks of every step of the video features: the HLS packager of the server
([video streaming](../user/video-streaming.md)) and the jobs of the media worker
([media worker](../user/media-worker.md)): OpenH264 decoding and encoding, scaling, renditions, poster and
thumbnails. They run with `make bench-media` (§5). The numbers are real and apply to the machine of §1.

## 1. Machine and inputs

| Item | Value |
|------|-------|
| Machine | Firecracker VM, 4 vCPU Intel Xeon @ 2.10 GHz, 15 GiB RAM (the machine of the [server benchmarks](README.md)) |
| Go | 1.27.2 |
| OpenH264 | 2.4.1, the Cisco build for linux/amd64 |
| Fixture | `internal/mediaworker/transcode/testdata/h264_aac_360p.mp4`: 640x360, 25 fps, 3 s, H.264 High with B-frames, AAC |
| 1080p video | 1920x1080, 25 fps, 20 s, H.264 High (libx264 `medium`, 6 Mbit/s, B-frames, a keyframe every 2 s) of a moving test pattern with film grain, AAC: `NEXO_BENCH_VIDEO` (§5); the grain makes it harder to decode than most camera footage |

Speeds marked `x` are seconds of video processed per second (2× processes a 10-minute video in 5 minutes);
`fps` are pictures per second.

## 2. Packager (server, per request)

`go test -bench . ./internal/service/media/`: the requests of a player on the 160x90 fixture, the object layer
replaced by a mock (its cost, a few µs per call, is included; real reads add the drive or network time).

| Request | Time | Allocated |
|---------|------|-----------|
| First request of a version: reading and indexing the `moov` (`Describe/cold`) | 279 µs (199 MB/s of index) | 88 KiB |
| Following requests, index cached by ETag (`Describe/cached`) | 27 µs | 5.7 KiB |
| Master playlist | 53 µs | 12 KiB |
| Media playlist | 29 µs | 7.3 KiB |
| Init segment | 21 µs | 5.4 KiB |
| Media segment (1 s, 13 KiB) | 116 µs (116 MB/s) | 139 KiB |

A segment is one ranged read of the source plus its fMP4 boxes: the server streams video at the speed of its
drives, with no transcoding.

## 3. Media worker, step by step

### OpenH264, one encoder or decoder on one core

`go test -bench . ./internal/mediaworker/openh264/`, on generated pictures with texture, motion and grain:

| Size | Encode | Decode |
|------|--------|--------|
| 240p (426x240) | 1717 fps | 3520 fps |
| 360p (640x360) | 844 fps | 1687 fps |
| 720p (1280x720) | 222 fps | 371 fps |
| 1080p (1920x1080) | 116 fps | 181 fps |

Decoding the 1080p video of §1 (High profile, CABAC, 6 Mbit/s of grain) is much slower than the generated
pictures: about 55 fps, which is what bounds a job (§4).

### Scaling (one picture, as a rendition does it)

| Scaling | Before | After | Δ |
|---------|--------|-------|---|
| 1080p → 720p | 16.6 ms | 4.4 ms | −74% |
| 1080p → 360p | 8.3 ms | 3.1 ms | −63% |
| 720p → 480p | 8.4 ms | 3.0 ms | −64% |
| 360p → 240p | 2.2 ms | 1.2 ms | −46% |
| 1080p → thumbnail tile (160x90) | 4.4 ms | 2.5 ms | −43% |

### Poster, thumbnails, parsing

| Step | 360p fixture (3 s) | 1080p video (20 s) |
|------|--------------------|--------------------|
| Parsing the MP4 (`Open`) | 79 µs | 270 µs |
| Poster (decode one keyframe, scale, JPEG) | 5.8 ms | 65 ms |
| Thumbnail sprite + WebVTT, one tile per second | 9 ms (330×) | 0.70 s (28×) |

The thumbnails decode only the keyframes they need, so they cost a fraction of the renditions.

## 4. Renditions, end to end

`BenchmarkRenditions`: decode the source once, scale and encode every rendition, write the MP4 files (audio
copied). Before/after are the commit of the media worker (`0322319`) and the optimizations of §6, `-count 4`,
compared with `benchstat` (p = 0.029).

| Renditions from the 1080p video | Before | After | Δ |
|---------------------------------|--------|-------|---|
| 240p | 14.8 s (1.36×) | 9.1 s (2.20×) | −38% |
| 360p | 14.1 s (1.42×) | 9.0 s (2.21×) | −36% |
| 720p | 19.7 s (1.02×) | 9.0 s (2.22×) | −54% |
| 720p + 360p + 240p | 21.8 s (0.92×) | 9.4 s (2.12×) | −57% |

From the 360p fixture: 240p at 18×, 360p at 30×.

Three renditions now cost what one does: the job is bound by decoding the source (one core), while scaling and
encoding run on the others. A 10-minute 1080p video with three renditions takes about 4.7 minutes on this machine;
`--concurrency` of the worker runs more jobs at once on more cores.

## 5. Reproduction

```console
$ make bench-media                                   # fetches OpenH264 into bin/openh264
$ NEXO_BENCH_VIDEO=$PWD/my-video.mp4 make bench-media    # renditions, poster and thumbnails of your own video
$ BENCH_COUNT=6 make bench-media > new.txt           # then benchstat old.txt new.txt
```

The 1080p video of §1:

```console
$ ffmpeg -f lavfi -i "testsrc2=size=1920x1080:rate=25:duration=20" -f lavfi -i "sine=frequency=440:duration=20" \
    -vf "noise=alls=12:allf=t" -c:v libx264 -profile:v high -preset medium -b:v 6M -g 50 -bf 3 -pix_fmt yuv420p \
    -c:a aac -b:a 128k -movflags +faststart bench1080.mp4
```

## 6. Optimizations made with these benchmarks

1. **Scaler** (`internal/mediaworker/transcode/scale.go`): the weights are computed once per rendition instead
   of once per picture and plane, in flat arrays; the buffers and the output picture are reused; the vertical
   pass runs first, row by row (contiguous reads, and when reducing it leaves fewer rows for the horizontal pass,
   whose reads are indexed); large planes are split into bands scaled in parallel. The output stays within ±1 of
   the first implementation on every sample (`TestScalerMatchesReference`). Allocations per rendition: −43…72%.
2. **Decoding ahead** (`internal/mediaworker/transcode/rendition.go`): the decoder runs on its own goroutine,
   a few pictures ahead, so decoding overlaps with scaling and encoding.
3. **Fixed**: the benchmarks found that the C calls of the OpenH264 binding could write their out parameters to
   a moved goroutine stack (`create encoder: 0` on a fresh goroutine). `call` is now `//go:uintptrescapes`, which
   keeps such arguments on the heap for the call (`TestFreshGoroutines`).

## 7. Open work

- Decoding is single-threaded: OpenH264 has a threaded decoder (`DECODER_OPTION_NUM_OF_THREADS`) not used yet;
  it would lift the bound of §4 on machines with more cores.
- The horizontal pass of the scaler and the colour conversion of the JPEGs are plain Go: SIMD assembly would
  speed them up further.
