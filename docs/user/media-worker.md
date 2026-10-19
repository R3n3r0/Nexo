# Media worker: renditions, posters, thumbnails and image variants

[Video streaming](video-streaming.md) plays the source file of a video as it is. The **media worker**
(`nexo-media-worker`) derives more from the videos a bucket rule selects:

- **renditions**: lower qualities (H.264/AAC) that the HLS master playlist offers next to the source, so
  players switch to them on slow networks;
- a **poster**: a JPEG frame shown before playback;
- **timeline thumbnails**: a sprite of small frames with its WebVTT map, for the previews of a seek bar.
- **image variants**: every image a rule selects in AVIF, WebP, JPEG and PNG at several widths, served in the
  best format for each client ([below](#images-every-format-and-width)).

The worker is a separate program, on the same machine or another one. Nexo never decodes video: it sends
jobs to the worker, and the worker reads the videos and writes its outputs through the S3 API, with
credentials of its own. A deployment without a worker keeps streaming the source files.

## How it works

```text
 PUT video ──► nexo ── job (notify_media_worker) ──► nexo-media-worker
                 ▲                                         │ GET the video (S3 API)
                 │                                         │ decode, scale, encode (OpenH264)
                 └──────── PUT the outputs (S3 API) ◄──────┘ .nexo-media/<key>/<version>/
```

1. A **media rule** of the bucket selects a video by prefix and suffix and says what to derive.
2. When such a video is created (PUT, multipart, copy) Nexo queues a job for the `notify_media_worker`
   target; when it is deleted, a cleanup job. The jobs travel through the persistent queue of the
   [event targets](events-lifecycle.md): a worker that is down receives them when it is back.
3. The worker stores every job on its disk (with `fsync`) before acknowledging it, so a restart of the worker
   loses none; it processes them, retries the transient failures with a backoff, and writes `status.json`
   in the sidecar directory of the version.
4. Nexo serves the outputs only while `status.json` says `done` **for the ETag of the current content**: an
   overwritten video never shows the renditions of the previous one.

The worker never modifies or deletes a video: it only writes under the reserved prefix `.nexo-media/` of the
bucket, and its [policy](#1-credentials-of-the-worker) can enforce it.

## No ffmpeg: OpenH264

The worker is a single Go program without ffmpeg. It decodes and encodes H.264 with
[OpenH264](https://www.openh264.org/), the codec Cisco publishes as open source (BSD) **and** as binaries
for which Cisco pays the MPEG LA H.264 patent royalties. The royalty-free use applies to the binaries that
the end user downloads from Cisco, so neither the worker nor its image ships the library: at its first start
the worker downloads the build of its platform from `ciscobinary.openh264.org` into its data directory and
checks its SHA-256 against the value pinned in the worker. Linux (amd64, arm64), macOS (amd64, arm64) and
Windows (amd64) are supported.

> OpenH264 Video Codec provided by Cisco Systems, Inc.

Without Internet access, run `nexo-media-worker fetch-openh264 --dir lib` on a connected machine of the same
platform (it downloads and verifies the library and prints its path), copy the file into the `lib` directory
of the data directory of the worker, or anywhere and pass its path with `--openh264`. A library you build from the sources works as well, but it is not covered by the
Cisco patent license.

## Supported videos

| | |
| --- | --- |
| Container | MP4 (`.mp4`, `.m4v`, `.mov`), not fragmented |
| Video | H.264 (AVC), any profile (B-frames included), 4:2:0 |
| Audio | AAC, copied as it is into the renditions |

A video the worker cannot process (H.265, another codec, a damaged file) gets `status.json` with state
`failed` and the reason, and is not retried; it still streams as a source file if the
[packager](video-streaming.md#supported-videos) supports it.

## Outputs

| Output | Content |
| --- | --- |
| `renditions/<name>.mp4` | H.264 Constrained Baseline + the AAC track of the source, progressive MP4, a keyframe every 2 seconds |
| `poster.jpg` | the keyframe at a tenth of the video (at most 10 seconds in), at most 1280 pixels wide |
| `thumbnails/sprite.jpg` | tiles 160 pixels wide, 10 per row, one per interval (at most 500) |
| `thumbnails/thumbnails.vtt` | WebVTT map of the sprite (`sprite.jpg#xywh=x,y,w,h`) |
| `status.json` | state of the job: `processing` (with the current step), `done` or `failed` |

The renditions are the presets below; those taller than the source are skipped (a 720p video gets no 1080p).
The width follows the aspect ratio of the source.

| Preset | Height | Video bitrate |
| --- | --- | --- |
| `2160p` | 2160 | 16 Mbit/s |
| `1440p` | 1440 | 8 Mbit/s |
| `1080p` | 1080 | 5 Mbit/s |
| `720p` | 720 | 2.8 Mbit/s |
| `480p` | 480 | 1.4 Mbit/s |
| `360p` | 360 | 800 kbit/s |
| `240p` | 240 | 400 kbit/s |

The outputs of a version live under `.nexo-media/<key>/<versionId>/` (`null` in an unversioned bucket).
The console and `nexoctl ls`, `find` and `tree` hide this prefix by default (`--media` shows it); the S3 API lists it like any other, so
backups, mirrors and replication keep the outputs with their videos. They count against the quota of the
bucket.

## Images: every format and width

An **image rule** makes the worker turn every image the rule selects into *variants*: the image at its own
size and at a set of widths, in every format of the rule. Clients then ask for the width they display, and Nexo
serves the most compact format they can decode: AVIF to recent browsers, WebP to the others, JPEG to everyone
else.

**The quick way: an image bucket.** Create the bucket with the **Images in every format** option and every
image uploaded to it gets AVIF, WebP, JPEG and PNG, at its own size and at 160, 320, 640, 1280, 1920 and 2560
pixels wide (only the widths smaller than the image: an image is never enlarged):

```console
$ nexoctl mb --with-images local/photos            # a new image bucket
$ nexoctl mb --with-hls --with-images local/media  # videos and images in the same bucket
$ nexoctl media rule add local/photos --images     # the same for an existing bucket
$ nexoctl media rule add local/shop --images --id products --prefix products/ --format webp,jpeg --width 320,800
```

In the console it is the switch of **Create bucket**; in the Admin API, `"images": true` in the body of
`POST /buckets` (with `admin:SetBucketMedia`). The option is the rule `{"id": "images", "images": {"formats":
["avif", "webp", "jpeg", "png"], "widths": [160, 320, 640, 1280, 1920, 2560]}}`. An image rule has no
renditions, poster or thumbnails; its default suffixes are those of the images below.

| | |
| --- | --- |
| Sources | JPEG, PNG, GIF (first frame), WebP, AVIF, HEIC/HEIF (iPhone photos), TIFF, BMP; recognized by their content, not by the extension |
| Formats | `avif`, `webp`, `jpeg`, `png` (formats of a rule, default all) |
| Widths | 16 to 8192 pixels, at most 16 per rule (default 160, 320, 640, 1280, 1920, 2560) |
| Orientation | the EXIF orientation of JPEG and TIFF photos is applied: the variants are upright |
| Metadata | removed: the variants carry no EXIF, GPS position or camera data |
| Transparency | kept in AVIF, WebP and PNG; JPEG is flattened onto white, so a transparent image is served as PNG to clients without AVIF or WebP |

The variants are stored next to the image, like the outputs of a video:
`.nexo-media/<key>/<versionId>/images/<width>.<ext>` and `images/full.<ext>` for the image at its own size, with
`status.json` listing them (`nexoctl media status` prints them by format).

### Serving the variants

| Route | For |
| --- | --- |
| `GET /api/v1/buckets/{bucket}/objects/image?key=...&w=640` | the console and Admin API clients (session or SigV4), needs `s3:GetObject` |
| `GET` or `HEAD /nexo/media/v1/{bucket}/image/{key}?w=640` | web pages and apps, on the S3 port: SigV4, presigned or a share link, like the [video streams](video-streaming.md) |

- `w`: the width the client displays; the narrowest variant at least that wide is served (the widest when none
  is). Without `w`, the image at its own size.
- `format`: `auto` (the default) picks the most compact format the `Accept` header names (AVIF, then WebP; JPEG
  for `*/*`), and the response says `Vary: Accept` for caches and CDNs; `avif`, `webp`, `jpeg` or `png` ask
  for one.
- `versionId`: a version of the image (`s3:GetObjectVersion`).
- Conditional requests (`If-None-Match`, `If-Modified-Since`) and ranges work; the ETag names the source, the
  format and the width.
- A variant exists only while `status.json` is done for the current content of the image: an overwritten image
  answers 404 until the worker has processed the new one.

A web page lets the browser choose:

```html
<img src="https://s3.example.com/nexo/media/v1/photos/image/beach.jpg?w=640"
     srcset="https://s3.example.com/nexo/media/v1/photos/image/beach.jpg?w=320 320w,
             https://s3.example.com/nexo/media/v1/photos/image/beach.jpg?w=1280 1280w"
     sizes="(max-width: 700px) 100vw, 640px" alt="The beach">
```

(for a private bucket, sign the URLs or use share links). `nexoctl share download --image local/photos/beach.jpg`
makes a revocable link that serves the image at its own size in the best format for whoever opens it; the
console offers the same in the share dialog of an image.

### Limits and memory

AVIF encoding is the slow and memory-hungry step (about 6 s and 1.2 GB for a 12-megapixel photo at its own
size, see [the benchmarks](https://github.com/R3n3r0/Nexo/blob/main/docs/bench/media.md)): a job of 28 variants
of such a photo takes about 11 seconds on 4 cores, 3 seconds without AVIF. The worker encodes the variants in
parallel within a memory budget, and never produces a variant that alone would not fit in it (an AVIF of a
very large image: the other formats serve it), so a large image cannot make it run out of memory.

| Flag | Environment | Default | |
| --- | --- | --- | --- |
| `--image-max-bytes` | `NEXO_WORKER_IMAGE_MAX_BYTES` | 256 MiB | largest image file processed |
| `--image-max-pixels` | `NEXO_WORKER_IMAGE_MAX_PIXELS` | 100 000 000 | largest image, read from its header before decoding (protects from decompression bombs) |
| `--image-memory` | `NEXO_WORKER_IMAGE_MEMORY` | 2 GiB | memory of the variants encoded at once |

A larger or undecodable image fails for good (`nexoctl media status` gives the reason); its source is never
modified.

## Setup

### 1. Credentials of the worker

Create a user for the worker with this policy (replace `*` in the resources with the buckets of your
rules to restrict it further): it reads the videos and writes only under `.nexo-media/`.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:GetObjectVersion"], "Resource": ["arn:aws:s3:::*/*"]},
    {"Effect": "Allow", "Action": ["s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion"],
     "Resource": ["arn:aws:s3:::*/.nexo-media/*"]},
    {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketVersions"], "Resource": ["arn:aws:s3:::*"],
     "Condition": {"StringLike": {"s3:prefix": [".nexo-media/*"]}}}
  ]
}
```

```console
$ nexoctl admin policy create local media-worker media-worker-policy.json
$ nexoctl admin user add local media-worker 'a-long-random-secret' --policy media-worker
```

### 2. Start the worker

With the binary:

```console
$ export NEXO_WORKER_S3_ENDPOINT=http://nexo.internal:9000
$ export NEXO_WORKER_ACCESS_KEY=media-worker NEXO_WORKER_SECRET_KEY='a-long-random-secret'
$ export NEXO_WORKER_AUTH_TOKEN="$(openssl rand -hex 32)"
$ nexo-media-worker --data-dir /var/lib/nexo-media-worker
level=INFO msg="OpenH264 loaded" version=2.4.1 license="OpenH264 Video Codec provided by Cisco Systems, Inc."
level=INFO msg="nexo-media-worker started" address=:9600 s3=http://nexo.internal:9000 concurrency=1
```

With Docker (build the image with `make docker-media-worker`; it downloads OpenH264 into its `/data` volume
at the first start):

```console
$ docker run -d --name nexo-media-worker -p 9600:9600 -v media-worker:/data \
    -e NEXO_WORKER_S3_ENDPOINT=http://nexo:9000 \
    -e NEXO_WORKER_ACCESS_KEY=media-worker -e NEXO_WORKER_SECRET_KEY='a-long-random-secret' \
    -e NEXO_WORKER_AUTH_TOKEN=... docker.io/paolor/nexo-media-worker
```

| Flag | Environment | Default | |
| --- | --- | --- | --- |
| `--s3-endpoint` | `NEXO_WORKER_S3_ENDPOINT` | | URL of the S3 API of Nexo (required) |
| `--access-key`, `--secret-key` | `NEXO_WORKER_ACCESS_KEY`, `NEXO_WORKER_SECRET_KEY` | | credentials of the worker (required) |
| `--auth-token` | `NEXO_WORKER_AUTH_TOKEN` | | bearer token Nexo sends (required) |
| `--address` | `NEXO_WORKER_ADDRESS` | `:9600` | address of the jobs endpoint (`POST /jobs`, `GET /healthz`) |
| `--data-dir` | `NEXO_WORKER_DATA_DIR` | `nexo-media-worker` | job queue, work files, downloaded library |
| `--concurrency` | `NEXO_WORKER_CONCURRENCY` | `1` | jobs processed at once (each one already uses every core) |
| `--max-attempts` | `NEXO_WORKER_MAX_ATTEMPTS` | `5` | attempts of a job that fails for a transient reason |
| `--openh264` | `NEXO_WORKER_OPENH264` | | path of the library (default: downloaded from Cisco) |
| `--s3-region` | `NEXO_WORKER_S3_REGION` | `us-east-1` | region of the signatures |
| `--name` | `NEXO_WORKER_NAME` | host name | name of the worker in `status.json` |
| `--log-level`, `--log-format` | `NEXO_WORKER_LOG_LEVEL`, `NEXO_WORKER_LOG_FORMAT` | `info`, `text` | logs (`json` for collectors) |

`nexo-media-worker healthcheck` probes a running worker (the `HEALTHCHECK` of the image). The data
directory needs room for the video being processed and its renditions.

### 3. Send the jobs to the worker

The `notify_media_worker` target delivers the jobs, with the keys of a [webhook](events-lifecycle.md)
(`endpoint`, `auth_token`, `client_cert`, `client_key`, `tls_skip_verify`, `queue_dir`, `queue_limit`):

```console
$ nexoctl admin config set local notify_media_worker enable=on endpoint=http://media-worker.internal:9600/jobs auth_token=...
```

`nexoctl admin notify targets test local _:media_worker` sends a test job, which the worker acknowledges
without processing anything. Unlike the event targets, this one needs no
bucket notification: the media rules decide which videos it receives.

### 4. Media rules

**The quick way: a video bucket.** Create the bucket with the **HLS video** option and every video uploaded to
it gets every rendition (240p to 2160p, up to the height of the video), a poster and thumbnails every 10
seconds, with no rule to write:

```console
$ nexoctl mb --with-hls local/videos               # a new video bucket
$ nexoctl media rule add local/videos --hls         # the same for an existing bucket
```

In the console it is the **HLS video** switch of **Create bucket**; in the Admin API, `"hls": true` in the body
of `POST /buckets` (it needs `admin:SetBucketMedia` besides `s3:CreateBucket`). The option is the rule
`{"id": "hls", "renditions": [every preset], "poster": true, "thumbnails": {"interval": "10s"}}`: change it or
add more rules as below.

**Rules of your own.** A bucket has up to 100 rules. A rule selects keys by `prefix` (empty: the whole bucket) and `suffixes`
(default `.mp4`, `.mov`, `.m4v`, matched without case); the first matching rule applies.

```json
{
  "rules": [
    {"id": "courses", "prefix": "courses/", "renditions": ["1080p", "720p", "360p"], "poster": true,
     "thumbnails": {"interval": "10s"}},
    {"id": "clips", "prefix": "clips/", "poster": true}
  ]
}
```

```console
$ nexoctl media set local/videos media.json
$ nexoctl media rule add local/videos --id clips --prefix clips/ --poster
$ nexoctl media get local/videos
```

In the console: bucket → **Media**. The rules need `admin:SetBucketMedia` to change and
`admin:GetBucketMedia` to read; they are part of the bucket metadata (cluster export and import included).

A rule applies to the videos written after it; to process the existing ones, or again after changing a rule:

```console
$ nexoctl media reprocess local/videos/courses/intro.mp4
$ nexoctl media status local/videos/courses/intro.mp4
Video      : local/videos/courses/intro.mp4
State      : done (current)
Rule       : courses
Source     : 1920x1080, 4m12s
Renditions : 1080p 1920x1080 5.1 Mbit/s, 720p 1280x720 2.9 Mbit/s, 360p 640x360 896 kbit/s
Poster     : yes
Thumbnails : 26 every 10s (160x90, 10 columns)
Updated    : 2026-10-19 13:04:06 UTC
```

## Serving the outputs

Once a video is `done`, its HLS master playlist lists the renditions after the source, with their
bandwidth, resolution and codecs: the console player, the [stream links](video-streaming.md#stream-links)
and any HLS player pick them automatically. The poster and the thumbnails have their own URLs, with the
authorization of `GetObject` on the video like the playlists:

| Path (S3 listener) | Content |
| --- | --- |
| `/nexo/media/v1/{bucket}/poster.jpg?key=…` | poster |
| `/nexo/media/v1/{bucket}/thumbnails.vtt?key=…` | WebVTT map, its URIs pointing to `sprite.jpg` (presigned when the request is) |
| `/nexo/media/v1/{bucket}/sprite.jpg?key=…` | thumbnail sprite |
| `/nexo/media/v1/{bucket}/status.json?key=…` | state of the job (also for videos never processed: `none`) |

The console serves the same files under `/api/v1/buckets/{bucket}/objects/hls/` and the state under
`/api/v1/buckets/{bucket}/objects/media/status` ([Admin API](reference/admin-api.md)).

## Deletions and versions

- Deleting a version of a video for good (a DELETE in an unversioned bucket, a DELETE with `versionId`, a
  lifecycle expiration) sends a cleanup job that removes its sidecar directory, every version of it.
- A delete marker keeps the version, so its outputs stay: they are still served for that `versionId`.
- In a versioned bucket the worker removes the versions of its own outputs that a new run replaced, so
  reprocessing a video does not accumulate old renditions.
- Only the videos a rule selects send cleanup jobs: after removing a rule, the outputs of its videos stay until
  you delete them (`nexoctl rm --recursive --force local/videos/.nexo-media/clips/`).

## Performance

Measured on a virtual machine with 4 vCPU Intel Xeon at 2.1 GHz (AVX-512, no SMT), 15.7 GiB of RAM, an ext4
virtio disk, Ubuntu 24.04, Go 1.27.2 and OpenH264 2.4.1, on a 20-second 1080p H.264 High video at 6 Mbit/s with film
grain ([machine and method](https://github.com/R3n3r0/Nexo/blob/main/docs/bench/media.md); `make bench-media` measures your own hardware and saves its description with the
results):

| Job (1080p H.264 source) | Speed |
|--------------------------|-------|
| 720p + 360p + 240p | 3.2× real time (a 10-minute video in about 3 minutes) |
| one rendition | 4–4.5× |
| poster | 44 ms |
| thumbnails, one per second | 74× real time |

A job uses every core: the video is split at its IDR pictures into spans of at least 2 seconds, each decoded and
encoded on its own core, and the spans are joined in order (the result is the same, byte for byte, on any number
of cores). The speed grows with the cores; a video shorter than 2 seconds, or with a single IDR picture, is one span
on one core.

## Scaling and security

- Each job decodes the video once, on every core (one span per core), and encodes its renditions in parallel;
  `--concurrency` runs several jobs at once, useful for many short videos. Scale a worker up (cores, `--concurrency`) rather than out: each
  `notify_media_worker:<id>` target receives every job, so two targets would process every video twice.
- The decoder parses untrusted files: run the worker as an unprivileged user (the image uses UID 65532),
  with resource limits, and with the least-privilege policy above. It needs no admin rights and no network
  access other than the S3 API of Nexo and, at the first start, `ciscobinary.openh264.org`.
- The jobs endpoint authenticates Nexo with the bearer token (compared in constant time); put it behind TLS
  (`client_cert`/`client_key` and a reverse proxy) when the network between Nexo and the worker is not trusted.
