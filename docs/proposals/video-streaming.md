# Proposal: video streaming (HLS), renditions and automatic subtitles

Status: **implemented** except the automatic subtitles. M1 (packager, share links, player), M2 (media worker,
renditions, poster, thumbnails, images) and M4 (the video behavior of the distributions and `nexo:GetMediaStream`)
are in the release; M3 (subtitles) and the subtitle editor of M4 are in progress. The user documentation is in
[video-streaming.md](../user/video-streaming.md), [media-worker.md](../user/media-worker.md) and
[distributions](../user/distributions/README.md#video-streaming-on-your-domain).

## 1. Summary

Let a video stored in Nexo be watched, shared and embedded like a video, not just downloaded:

1. **HLS packaging on the fly, inside `nexo`.** For an MP4 already in a bucket, Nexo serves an HLS playlist and
   its fMP4 segments by re-packaging the samples of the file. No transcoding, no new files, no external tools.
2. **A separate media worker** (`nexo-media-worker`, its own container) that reacts to bucket events and writes,
   next to the video:
   - lower-quality **renditions** for adaptive bitrate;
   - a **poster** and timeline thumbnails;
   - **subtitles** generated automatically from the audio and translated into the languages the bucket asks for.
3. **Sharing and playback**: a "share as video" link, a player page and an embed snippet. The playlist carries every
   rendition and every subtitle language, and the viewer picks one in the player.

The server stays a single Go binary that never decodes media. Everything CPU- or GPU-heavy (ffmpeg, speech
recognition, translation) runs in the worker, which talks to Nexo only through the public S3 API and the event
notifications that already exist. A deployment that does not run the worker still gets step 1.

## 2. Why

- Browsers already play a progressive MP4 with Range requests, and Nexo supports them. HLS adds what Range cannot:
  **adaptive bitrate** on slow or mobile links, **subtitle tracks**, and compatibility with HLS players (smart TVs,
  apps, hls.js, Video.js, Safari/iOS native).
- Self-hosters keep videos in object storage (family archives, courses, product videos, CCTV exports) and today
  need a second system to stream them (Jellyfin, PeerTube, a CDN with a media packager).
- Nexo already has the pieces around it: distributions on custom domains with ACME certificates, a cache, signed
  URLs, managed share links, bucket event notifications with a persistent queue, and a console with a video preview.
- Automatic subtitles in several languages make shared videos accessible and are rare in self-hosted storage.

## 3. Goals and non-goals

Goals:

- Stream any H.264/H.265 + AAC MP4/MOV object as HLS (fMP4/CMAF segments) without re-encoding.
- Optional adaptive bitrate through renditions produced outside the server.
- Automatic subtitles: transcription in the spoken language, machine translation to configured languages, WebVTT
  tracks in the HLS playlist, clearly labelled as automatic.
- Private by default: transcription and translation run locally in the worker unless the operator configures a cloud
  provider.
- Works with the existing security model: policies, access rules, block public access, managed shares, signed URLs.

Non-goals (for this proposal):

- Live streaming (RTMP/WebRTC/LL-HLS ingest).
- DRM (FairPlay, Widevine). AES-128 HLS encryption could come later.
- DASH. CMAF segments would make it cheap to add later, but HLS is played everywhere.
- Dubbing or translated audio tracks.
- Transcoding inside `nexo`.

## 4. Architecture

```
            upload                     event (s3:ObjectCreated:*)
 client ───────────► nexo ──────────────────────────────────► nexo-media-worker
                      │  ▲                                       │  ffmpeg: renditions, poster, thumbnails
                      │  │  S3 API: GET source, PUT sidecars     │  whisper: transcription
                      │  └───────────────────────────────────────┘  translator: subtitles in N languages
                      │
 viewer ◄──── HLS ────┘  packager: master.m3u8, media playlists, fMP4 segments (from source and renditions),
                         WebVTT playlists; cached by the distribution cache
```

### 4.1 In `nexo`: the packager (new service `media`)

A new service, following [docs/12](../12-service-architecture.md) (interface + implementation, routes as a
service):

- **MP4 index**: reads the `moov` box of the object with Range requests (it may sit at the end of the file),
  builds a compact index (tracks, codecs, sample tables, sync samples) and stores it as a sidecar object, so later
  requests do not parse the file again. Fragmented MP4 is read through its `moof` boxes. Library: a pure-Go MP4
  parser (for example `github.com/Eyevinn/mp4ff`, MIT), wrapped behind the service interface.
- **Segmenting**: segments of about 6 s, cut at sync samples (keyframes). A source with a long GOP gets longer
  segments; the playlist states the real durations.
- **Serving**:
  - `master.m3u8`: one variant per rendition (the source, plus each rendition the worker produced), audio
    attributes, and one `EXT-X-MEDIA TYPE=SUBTITLES` entry per subtitle language.
  - a media playlist per variant (`EXT-X-VERSION:7`, `EXT-X-MAP` init segment, `#EXTINF` segments);
  - init and media segments built on request from byte ranges of the object;
  - WebVTT subtitle playlists, with the `.vtt` files cut into segments aligned to the video, so players do not
    load a whole file at once.
- **Cache**: segments are deterministic for (bucket, key, version, rendition, segment), so the existing
  distribution cache stores them like any object. The cache key includes the version ID: overwriting a video
  changes every URL.
- **Limits**: maximum index size, maximum tracks and sample count, timeouts; the parser is fuzzed like the
  SigV4 and XML parsers. The parser reads untrusted files inside the server, which is why it only parses
  containers and never decodes.

What the server never does: decode or encode audio/video, run external programs, call cloud APIs.

### 4.2 URLs and authorization

Playlists use relative URIs, so authorization must cover a whole tree of URLs, not a single object. Three ways in:

| Access | URL | Authorization |
|--------|-----|---------------|
| Signed in (console, API) | `/nexo/media/v1/b/<bucket>/<key>/master.m3u8` | session or SigV4; `s3:GetObject` on the source key |
| Share link | `/nexo/media/v1/s/<token>/master.m3u8` | a **media share**: a managed share whose token covers the playlists, segments and subtitles of one video, with expiry and revocation like the existing shares |
| Distribution | `https://videos.example.com/<key>/master.m3u8` (path configurable) | the distribution's rules: public, signed URLs or key groups; the signature in the first path segment covers its children |

Encrypted objects: SSE-S3 and SSE-KMS work, since the packager reads through the object layer. **SSE-C objects
are not streamable**, because every segment request would need the customer key. The API answers with a clear
error.

### 4.3 Configuration: per bucket or prefix

A new bucket configuration, stored with the other bucket metadata (exported by `admin cluster export` like
lifecycle):

```json
{
  "rules": [
    {
      "id": "courses",
      "prefix": "courses/",
      "suffixes": [".mp4", ".mov"],
      "renditions": ["1080p", "720p", "480p"],
      "poster": true,
      "thumbnails": { "interval": "10s" },
      "subtitles": {
        "auto": true,
        "sourceLanguage": "auto",
        "translateTo": ["en", "it", "es", "de"]
      }
    }
  ]
}
```

```bash
nexoctl media set local/videos media.json
nexoctl media info local/videos/courses/intro.mp4         # tracks, renditions, subtitles, job status
nexoctl media reprocess local/videos/courses/intro.mp4 --subtitles --translate-to fr
nexoctl media share local/videos/courses/intro.mp4 --expire 7d   # prints the player link and the m3u8 link
```

The console gets a "Video" tab on the bucket (the rules), and the object drawer shows the player, the processing
status and the subtitle tracks.

Without a rule, streaming the source still works (4.1). Rules only drive the worker.

### 4.4 Sidecar layout

The worker writes next to nothing it does not own: all derived files live under a reserved prefix of the same
bucket, keyed by the **version ID** of the source:

```
.nexo-media/<key>/<versionId>/
  index.bin                  # MP4 index (written by nexo)
  status.json                # job state, written by the worker
  renditions/720p.mp4        # H.264/AAC, closed GOP of 2 s, so segments line up across renditions
  renditions/480p.mp4
  poster.jpg
  thumbnails/sprite.jpg + thumbnails.vtt
  subtitles/it.vtt           # source language (transcription)
  subtitles/en.vtt           # translations
  subtitles/manifest.json    # language, label, auto/edited, model, created
```

- Listings in the console and in `nexoctl ls` hide `.nexo-media/` by default (a new flag shows it); the S3 API
  lists it like any prefix, so backups and `mirror` keep working.
- Deleting or overwriting the source triggers a cleanup by the worker; a scanner pass also removes sidecars whose
  source version no longer exists.
- Quota and usage count the sidecars, and the console shows them separately ("Video data").
- No new database: the state is objects, so it replicates, heals and exports like the rest.

### 4.5 The media worker

A separate Go program, `nexo-media-worker`, shipped as `paolor/nexo-media-worker` (CPU) and
`paolor/nexo-media-worker:cuda` (NVIDIA GPU):

- **Input**: bucket notifications. A webhook endpoint for small setups. For durability and several workers, a
  queue target that Nexo already supports (NATS, Kafka, Redis, AMQP). Nexo's persistent event queue covers worker
  downtime.
- **Credentials**: an access key with a policy limited to `GetObject` on the media prefixes and `PutObject` /
  `DeleteObject` on `.nexo-media/*`. It uses only the S3 API: no internal RPC, no admin rights.
- **Jobs**: idempotent, keyed by `bucket/key/versionId + rule hash`. Retries with backoff, a concurrency limit
  per worker, and cancellation when the source version is deleted. Progress goes to `status.json`, which the
  console polls, and metrics are exposed for Prometheus.
- **Steps** (each skipped when its output exists and is current):
  1. probe (`ffprobe`): codecs, duration, audio streams;
  2. renditions (`ffmpeg`, H.264/AAC, fixed 2 s GOP, at most the source resolution), plus a passthrough-friendly
     remux when the source has a long GOP or an exotic container;
  3. poster and thumbnail sprite;
  4. subtitles (4.6).
- **Isolation**: ffmpeg and the speech models parse untrusted media, so they run only in the worker. The worker
  runs read-only, without root, with seccomp, CPU and memory limits, and network access to Nexo (and the
  translation endpoint, if one is configured) only.

### 4.6 Automatic subtitles in several languages

Pipeline, per video:

1. **Audio**: `ffmpeg` extracts the first audio track (or the one tagged with the configured language) as 16 kHz
   mono.
2. **Transcription**: [Whisper](https://github.com/openai/whisper) models through **whisper.cpp** (MIT) or
   **faster-whisper** (MIT). Voice activity detection skips silence. The spoken language is detected unless the
   rule sets it. Output: cues with start/end times, in the source language.
3. **Cue shaping**: lines of at most about 42 characters, at most 2 lines, a minimum duration, no overlaps. Cues
   are split at sentence boundaries where possible.
4. **Translation** into each language of `translateTo`, through a pluggable backend:

   | Backend | Runs | Notes |
   |---------|------|-------|
   | `libretranslate` (default) | self-hosted LibreTranslate / Argos Translate | OPUS-MT models (CC-BY 4.0), offline, CPU-friendly, quality varies by language pair |
   | `openai-compatible` | any OpenAI-compatible endpoint: local (Ollama, vLLM, llama.cpp server) or hosted | best quality on context; the worker sends whole sentences with neighbouring cues as context and asks for the same number of cues back |
   | `deepl`, `anthropic` | hosted APIs | opt-in; audio never leaves the worker, only text |

   Translation works on whole sentences, then maps the text back onto the source cue timings, so the translated
   subtitles stay in sync. Languages Whisper hears natively can also be transcribed directly instead of translated.
5. **Output**: one WebVTT file per language (BCP-47 codes), and an entry in `subtitles/manifest.json` with
   `auto: true`, the model and backend used, and the date.
6. **In the playlist**: each language becomes an `EXT-X-MEDIA TYPE=SUBTITLES` entry. `NAME` is the language name
   with "(auto)" for machine-generated tracks, `LANGUAGE` is the BCP-47 code, and `DEFAULT`/`AUTOSELECT` follow
   the viewer's browser language when the player supports it.

Human in the loop:

- Upload your own `.vtt` or `.srt` (converted to WebVTT) for any language, from the console or with
  `nexoctl media subtitles put`. A manual track replaces the automatic one and is never overwritten by the worker.
- Later: a subtitle editor in the console (fix text and timings, with the video beside it). Editing marks the
  track `auto: false`.

Privacy and licences:

- The default setup (whisper.cpp + LibreTranslate) runs fully offline, and no audio or text leaves the operator's
  machines. Hosted backends are explicit opt-ins in the worker configuration.
- Models are downloaded by the worker image or on first start, never bundled in `nexo`. Whisper weights are MIT;
  OPUS-MT models are CC-BY 4.0, so the worker documentation credits them. NLLB-200 is **not** a default because its
  licence (CC-BY-NC) forbids commercial use.

Rough cost, to be measured on real hardware before any number is published: transcription is the heavy part.
Small Whisper models run on CPU; large models want a GPU. Translation with OPUS-MT is cheap; with an LLM it
depends on the endpoint. The worker reports processing time per minute of video in its metrics.

## 5. Player and sharing

- **Share page**: `/nexo/media/v1/s/<token>/` serves a small page with a player (hls.js, bundled with the console
  assets): quality selector, subtitle menu, poster, thumbnail scrubbing, keyboard shortcuts. Safari uses native HLS.
- **Embed**: an `<iframe>` snippet, and the raw `master.m3u8` link for any HLS player.
- **Console**: the object preview uses the same player when a video has HLS available, and the Share dialog gets a
  "Video link" option next to the download link.
- **Distributions**: a "video" behaviour serves `master.m3u8` under the distribution's domain, with cache and
  signed URLs, as a self-hosted alternative to a CDN with a media packager.

## 6. Security

- New code that parses untrusted input in the server: the MP4 index parser. It is container-only, memory-safe Go,
  has hard limits, and runs in the fuzzing CI job.
- Media share tokens follow the managed share model: scoped to one video version, expiring, revocable, audited.
- Policies: streaming requires `s3:GetObject` on the source. A new action, `nexo:GetMediaStream`, lets a policy
  allow streaming without allowing the download of the original. Access rules and block public access apply to
  the media paths as to the object.
- The worker has least-privilege credentials (4.5) and never gets admin rights.
- Threat model update: decompression bombs and pathological files (handled by limits in nexo, timeouts and
  resource limits in the worker), and the worker as a confused deputy (its policy cannot read outside the media
  prefixes).

## 7. Testing

- Packager: golden playlists and segments for a corpus of sample files (faststart and not, fragmented, H.265,
  long GOP, several audio tracks, variable frame rate); fuzzing of the parser; `ffprobe`/`ffmpeg -i master.m3u8`
  checks in CI that the output decodes.
- Playback: Playwright tests that play the share page with hls.js in Chromium, switch quality, enable a subtitle
  track and check the cue text.
- Worker: integration tests against a real Nexo with tiny models (whisper `tiny`, a stub translator) and short
  clips with known speech; idempotency and cleanup on delete and overwrite.
- Cluster: segments served by any node, cache invalidation on overwrite, and SSE-S3/KMS sources.

## 8. Plan

| Milestone | Content | Size |
|-----------|---------|------|
| M1 | Packager for the source file, media share links, player page, console player, `nexoctl media share/info` | 2–3 weeks |
| M2 | Media worker: events, renditions, poster and thumbnails, `status.json`, bucket media rules in API, CLI and console | 2–3 weeks |
| M3 | Automatic subtitles: whisper.cpp, translation backends, WebVTT tracks in HLS, manual `.vtt`/`.srt` upload | 2–3 weeks |
| M4 | Distribution "video" behaviour, `nexo:GetMediaStream`, subtitle editor in the console | 2 weeks |

Each milestone is usable on its own: M1 alone already lets people share a playable video link.

## 9. Open questions

1. Reserved prefix `.nexo-media/` in the same bucket, or a companion bucket per bucket? Same-bucket keeps quota,
   replication and export simple. A companion bucket keeps listings cleaner.
2. Should `nexo` itself generate the poster from the first keyframe (decoding one image), or leave all decoding to
   the worker? The proposal says worker only.
3. Default translation backend: LibreTranslate (offline, varying quality) or an OpenAI-compatible local LLM
   (better quality, needs a GPU for speed)?
4. Do we need per-viewer analytics (views, watch time), or only the existing access logs and metrics?
5. AES-128 encrypted HLS for private videos, or are short-lived share tokens enough?

## 10. Decisions taken in M2

- **OpenH264 instead of ffmpeg.** The worker is a single Go program: it parses MP4 with mp4ff, decodes and
  encodes H.264 with the Cisco build of OpenH264 loaded at run time (purego, no cgo), scales in Go and writes JPEG
  with the standard library. The library is downloaded from Cisco at the first start (its patent license covers
  the binaries end users download) and verified against a pinned SHA-256. The renditions are H.264 Constrained
  Baseline (OpenH264 has no B-frames or CABAC), with the AAC track of the source copied; H.265 sources fail as
  unsupported and still stream as source files.
- **Delivery**: a dedicated `notify_media_worker` target (webhook semantics, persistent queue) instead of
  bucket notifications to a generic target: the media rules decide which videos produce jobs, and the jobs carry
  the matching rule. The worker acknowledges a job only after writing it durably to its own disk queue.
- **Open question 1**: the reserved prefix `.nexo-media/` in the same bucket, hidden by the console and by
  `nexoctl ls` by default.
- **Open question 2**: the server never decodes; the poster comes from the worker.
- **Versioned buckets**: the worker deletes the replaced versions of its own outputs and, on a permanent delete
  of a video version, every version of its sidecar directory.
- **Deferred**: the scanner sweep of orphaned sidecars (after a rule is removed), the separate "Video data"
  usage figure, the CUDA image and metrics of the worker.
