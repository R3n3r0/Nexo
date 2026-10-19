# Media pipeline performance

Go benchmarks of every step of the video features: the HLS packager of the server
([video streaming](../user/video-streaming.md)) and the jobs of the media worker
([media worker](../user/media-worker.md)): OpenH264 decoding and encoding, scaling, renditions, poster and
thumbnails; and of the image variants of the media worker (§8). They run with `make bench-media` (§5). The
numbers are real and apply to the machine of §1.

## 1. Machine and inputs

Every number on this page was measured on this machine (the output of `test/bench/machine.sh`, which
`make bench-media` saves with every run):

| Item | Value |
|------|-------|
| Date | 2026-10-19 |
| CPU | Intel Xeon @ 2.10 GHz (x86_64), server class; the hypervisor does not expose the exact model |
| Cores | 4 vCPU: 1 socket × 4 cores × 1 thread (no SMT) |
| Caches | L1d 48 KiB per core, L2 2 MiB per core, L3 260 MiB shared |
| Instruction sets | AVX2, AVX-512 (F, BW), BMI2, SHA-NI, GFNI, VAES |
| Virtualization | Firecracker micro-VM on KVM, shared host: expect some noise between runs |
| RAM | 15.7 GiB |
| OS | Ubuntu 24.04.5 LTS, Linux 6.18.44 |
| Disk | ext4 on a virtio block device (`/dev/vda`): the work files of the renditions |
| Go | 1.27.2 linux/amd64, GOMAXPROCS 4 |
| OpenH264 | 2.4.1, the Cisco build `libopenh264-2.4.1-linux64.7.so` |

| Input | Description |
|-------|-------------|
| 360p fixture | `internal/mediaworker/transcode/testdata/h264_aac_360p.mp4`: 640x360, 25 fps, 3 s, H.264 High with B-frames, AAC |
| 1080p video | 1920x1080, 25 fps, 20 s, H.264 High (libx264 `medium`, 6 Mbit/s, B-frames, a keyframe every 2 s) of a moving test pattern with film grain, AAC, made with ffmpeg 6.1.1 (§5) and passed in `NEXO_BENCH_VIDEO`; the grain makes it harder to decode than most camera footage |

The numbers depend on the machine: the single-core steps (encoding, decoding) follow the clock and the
microarchitecture, the renditions of §4 the number of cores until decoding is the bound. Run `make bench-media` on
your own hardware to size a worker.

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
pictures: about 55 fps on one core. A job therefore decodes on every core at once (§4, §6).

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
| Poster (decode one keyframe, scale, JPEG) | 5.8 ms | 44 ms |
| Thumbnail sprite + WebVTT, one tile per second | 9 ms (330×) | 0.27 s (74×; 0.70 s, 28×, on one core) |

The thumbnails decode only the keyframes they need, each with its own decoder, on every core: they cost a
fraction of the renditions.

## 4. Renditions, end to end

`BenchmarkRenditions`: decode the source, scale and encode every rendition, write the MP4 files (audio copied).
Three versions of the code, each step of §6 on top of the previous one: the commit of the media worker
(`0322319`, `-count 4`), the scaler and the decoding ahead (`4e57463`, `-count 4`, `benchstat` p = 0.029), the
video split into spans encoded on every core (`-count 3`, median).

| Renditions from the 1080p video | M2 | Scaler, decoding ahead | Spans on every core | Δ total |
|---------------------------------|----|------------------------|---------------------|---------|
| 240p | 14.8 s (1.36×) | 9.1 s (2.20×) | 4.4 s (4.52×) | −70% |
| 360p | 14.1 s (1.42×) | 9.0 s (2.21×) | 4.4 s (4.58×) | −69% |
| 720p | 19.7 s (1.02×) | 9.0 s (2.22×) | 5.1 s (3.96×) | −74% |
| 720p + 360p + 240p | 21.8 s (0.92×) | 9.4 s (2.12×) | 6.3 s (3.18×) | −71% |

From the 360p fixture (one span: it has a single IDR picture after the first one): 240p at 18×, 360p at 30×.

With one decoder per span the job uses all 4 cores: decoding, scaling and encoding are all parallel, and the
numbers grow with the cores (on this machine the four cores are busy for the whole job). A 10-minute 1080p video
with three renditions takes about 3.1 minutes; `--concurrency` of the worker runs more jobs at once, which helps
when the videos are short (a video shorter than 2 s is a single span).

## 5. Reproduction

```console
$ make bench-media                                   # fetches OpenH264 into bin/openh264
$ NEXO_BENCH_VIDEO=$PWD/my-video.mp4 make bench-media    # renditions, poster and thumbnails of your own video
$ NEXO_BENCH_IMAGE=$PWD/my-photo.heic make bench-media  # the image job of §8 on your own image
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
3. **Spans on every core** (`internal/mediaworker/transcode/span.go`, `rendition.go`): the video is split at
   its IDR pictures into spans that decode alone (a span starts at an IDR picture and none of its pictures is
   shown after one of the next span, checked on the presentation times). Each span is decoded by its own
   decoder and encoded by its own encoders on a worker goroutine, one per core (fewer for very large videos,
   within 1 GiB of pictures), and the encoded spans are appended to the files in order, at most two per worker
   ahead of the file. A span starts where a single pass puts an IDR picture anyway (the first picture of a
   2-second period), so the renditions keep an IDR picture every 2 s, aligned across renditions; the output
   does not depend on the number of workers, byte for byte (`TestRenditionsInParallel`). The keyframes of the
   thumbnails are decoded in parallel the same way.
4. **Fixed**: the benchmarks found that the C calls of the OpenH264 binding could write their out parameters to
   a moved goroutine stack (`create encoder: 0` on a fresh goroutine). `call` is now `//go:uintptrescapes`, which
   keeps such arguments on the heap for the call (`TestFreshGoroutines`).

## 7. Threads of the OpenH264 decoder (not used)

OpenH264 2.4.1 also has a threaded decoder (`DECODER_OPTION_NUM_OF_THREADS`, at most 3 threads, one picture
per thread). It was measured and left out:

- 1 thread: the same pictures as no thread, only 5% faster (parsing moves off the calling thread).
- 2 or 3 threads: the pictures differ from those of the single-threaded decoder (about 47 dB PSNR), and with
  B-frames they differ from one run to the next; with 3 threads a picture went missing and the decoder hung.

The spans of §6 use every core with the single-threaded decoder, whose output is exact.

## 8. Images

`go test -bench . ./internal/mediaworker/imaging/` (part of `make bench-media`): the variants an image rule
asks for. The input is a generated 12-megapixel picture (4000x3000, the size of a phone camera) with the
structure of a photograph: smooth gradients, shapes with sharp edges, and Gaussian sensor noise (σ = 4), which
makes it harder to compress than most photos (the noise is averaged away by the smaller widths). It is encoded
as JPEG (quality 90, 2.4 MB), PNG, WebP, AVIF and HEIC to measure each decoder. `MP/s` are megapixels of the
source per second.

### Decoding (12 MP, one image)

| Source | Time | Speed | Decoder |
|--------|------|-------|---------|
| JPEG | 178 ms | 68 MP/s | `image/jpeg`, kept as YCbCr |
| PNG | 445 ms | 27 MP/s | `image/png` |
| WebP (lossy) | 366 ms | 33 MP/s | `golang.org/x/image/webp` |
| HEIC | 1.31 s | 9.1 MP/s | `github.com/gen2brain/h265` (pure Go) |
| AVIF | 2.28 s | 5.3 MP/s | `github.com/gen2brain/avif` (dav1d in WebAssembly, one core) |

### Resizing (12 MP source, every core)

Area averaging (each output pixel is the average of the source area it covers) on premultiplied RGBA, or on
the YCbCr planes of a JPEG converted to RGBA after the resize (2.7 times less data to read). The weights are
computed once per size and the rows are split into bands, one per core (`internal/mediaworker/resample`, shared
with the renditions of the videos).

| Width | From RGBA | From YCbCr (JPEG) |
|-------|-----------|-------------------|
| 2560 | 77 ms | 59 ms |
| 1280 | 32 ms | 31 ms |
| 640 | 22 ms | 16 ms |
| 160 | 17 ms | 14 ms |
| Own size (YCbCr → RGBA only) | — | 42 ms |

Turning the 12 MP picture upright (EXIF orientation 6, a 90° rotation) takes 66 ms; the smaller widths are
resized first and turned after, which costs less.

### Encoding (one variant, one core)

| Format | 4000x3000 (own size) | 1920x1440 | 640x480 | Settings |
|--------|----------------------|-----------|---------|----------|
| AVIF | 5.95 s, 537 KiB | 1.19 s, 24 KiB | 146 ms, 6.6 KiB | quality 60, speed 9, 4:2:0 (aom in WebAssembly) |
| WebP | 2.69 s, 730 KiB | 402 ms, 32 KiB | 44 ms, 6.0 KiB | lossy, quality 80, method 2 (libwebp translated to Go) |
| JPEG | 344 ms, 1569 KiB | 73 ms, 189 KiB | 8.7 ms, 18 KiB | quality 85, alpha flattened onto white |
| PNG | 1.87 s, 15.2 MiB | 368 ms, 2.7 MiB | 36 ms, 270 KiB | default compression (lossless) |

The settings were chosen with these benchmarks: AVIF speed 9 is 3 to 6 times faster than speed 6 for the same
size (speed 10 is not faster in WebAssembly), WebP method 2 is 3 times faster than the default method 4 for the
same size, and PNG `BestSpeed` saves only 20% of the time for larger files.

### The job of one image

`BenchmarkJob`: decode, then the image at its own size and every default width smaller than it (160, 320,
640, 1280, 1920, 2560), each in every format of the rule, encoded in parallel on every core.

| Source (12 MP) | Formats | Variants | Time |
|----------------|---------|----------|------|
| JPEG | AVIF, WebP, JPEG, PNG (the default) | 28 | 11.3 s |
| JPEG | WebP, JPEG | 14 | 3.2 s |
| HEIC | AVIF, WebP, JPEG, PNG | 28 | 15.4 s |
| HEIC | WebP, JPEG | 14 | 4.2 s |

AVIF is most of the cost: its variant at the image's own size takes 6 s on one core, and the job cannot end
before it (it starts first). A rule that does not need AVIF (or the image at its own size in AVIF) is 3 to 4
times faster. On this machine 4 encoders at once run only about twice as fast as one (the four vCPU of the
shared host do not scale further, §1), so a machine with real cores does better.

**Memory.** The encoders need, at their peak, about 100 bytes per pixel for AVIF (aom keeps its own buffers in
the WebAssembly memory: 1.2 GB for the 12 MP image at its own size), 26 for WebP, 6 for PNG and 4 for JPEG. The
worker starts a variant only when its estimate fits in its memory budget (2 GiB by default, `ImageMemory` of
the worker configuration), so a 12 MP image with every format peaks at about 2 GB; a variant larger than the
whole budget runs alone. Images above 100 megapixels (read from the header, before decoding) or 256 MiB are
refused for good, as decompression bombs.

## 9. Open work

- The horizontal pass of the scaler and the colour conversion of the JPEGs are plain Go: SIMD assembly would
  speed them up further.
