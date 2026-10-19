# Media worker: renditions, posters and thumbnails

[Video streaming](video-streaming.md) plays the source file of a video as it is. The **media worker**
(`nexo-media-worker`) derives more from the videos a bucket rule selects:

- **renditions**: lower qualities (H.264/AAC) that the HLS master playlist offers next to the source, so
  players switch to them on slow networks;
- a **poster**: a JPEG frame shown before playback;
- **timeline thumbnails**: a sprite of small frames with its WebVTT map, for the previews of a seek bar.

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
| `--concurrency` | `NEXO_WORKER_CONCURRENCY` | `1` | jobs processed at once (each uses about one core per rendition) |
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

A bucket has up to 100 rules. A rule selects keys by `prefix` (empty: the whole bucket) and `suffixes`
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

In the console: bucket → **Video**. The rules need `admin:SetBucketMedia` to change and
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

## Scaling and security

- Each job decodes the video once and encodes its renditions in parallel, one core each; `--concurrency`
  runs several jobs at once. Scale a worker up (cores, `--concurrency`) rather than out: each
  `notify_media_worker:<id>` target receives every job, so two targets would process every video twice.
- The decoder parses untrusted files: run the worker as an unprivileged user (the image uses UID 65532),
  with resource limits, and with the least-privilege policy above. It needs no admin rights and no network
  access other than the S3 API of Nexo and, at the first start, `ciscobinary.openh264.org`.
- The jobs endpoint authenticates Nexo with the bearer token (compared in constant time); put it behind TLS
  (`client_cert`/`client_key` and a reverse proxy) when the network between Nexo and the worker is not trusted.
