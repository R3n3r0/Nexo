# Video streaming

Nexo plays the MP4 videos of a bucket as **HLS streams**, packaged on the fly: there is no transcoding
and nothing to prepare. A video uploaded with `nexoctl cp`, the console or any S3 client plays at once
in the console, and a **stream link** shares it with people who have no account, in a browser or in any
HLS player.

## Supported videos

| | |
| --- | --- |
| Container | MP4 and its variants (`.mp4`, `.m4v`, `.mov`), with the index (`moov`) at the start or at the end |
| Video | H.264 (AVC) or H.265 (HEVC) |
| Audio | AAC, optional |

Fragmented MP4 files, objects encrypted with SSE-C and other codecs are refused with `400`: share
them as files. Objects encrypted with SSE-S3 or SSE-KMS stream like the others.

The segments are cut at the keyframes of the video, about every 6 seconds: a video encoded with a
keyframe every 1-2 seconds (the default of most encoders and cameras) seeks precisely.

The source plays as it is. For lower qualities that players switch to on slow networks, a poster and
timeline thumbnails, run the [media worker](media-worker.md): the master playlist then lists its renditions
next to the source.

## In the console

The preview of an MP4 video in the object browser is an HLS stream: playback starts at once and
seeking does not download the file. Browsers without HLS support fall back to the file.

## Stream links

A stream link is a [managed share link](security/sharing.md): it belongs to its owner, expires
(at most 7 days) and is revoked like any share. It gives two URLs:

- the **player page** of the server, for people: `https://s3.example.com/nexo/media/player/#/nexo/media/v1/…`;
- the **HLS playlist**, for players (VLC, Safari, mobile apps) and for pages that embed the video with
  [hls.js](https://github.com/video-dev/hls.js): `https://s3.example.com/nexo/media/v1/<bucket>/master.m3u8?…`.

In the console, **Share** on a video offers **Stream as a video** (on by default for MP4 files). With
the CLI:

```console
$ nexoctl share download --stream --expire 7d local/videos/talk.mp4
URL: local/videos/talk.mp4
Expire: 7d
Share: https://s3.example.com/nexo/media/v1/videos/master.m3u8?key=talk.mp4&X-Amz-Algorithm=…
Player: https://s3.example.com/nexo/media/player/#/nexo/media/v1/videos/master.m3u8?key=talk.mp4&X-Amz-Algorithm=…
ID: 2xk4m7q9fz3a (revoke with: nexoctl share revoke local 2xk4m7q9fz3a)
```

Revoking the share stops the playback: the playlists and the segments are signed by the share, until
it expires. A link pins the version of the video it was created for: when the object is overwritten, an
open player stops (`412`) and a new link plays the new version.

The player page and the playlists are served by the S3 listener, so a stream link works when only
the S3 API is exposed to the Internet. The page holds no data: the playlist travels in the fragment
of its URL (after `#`), which browsers never send to a server.

## HLS API

The streams follow the authorization of `GetObject`: whoever may read an object may stream it.

| Path (S3 listener) | Content |
| --- | --- |
| `/nexo/media/v1/{bucket}/master.m3u8?key=…[&versionId=…]` | master playlist |
| `/nexo/media/v1/{bucket}/media.m3u8?key=…&rev=…` | media playlist (VOD) |
| `/nexo/media/v1/{bucket}/init.mp4?key=…&rev=…` | fMP4 initialization segment |
| `/nexo/media/v1/{bucket}/segment.m4s?key=…&rev=…&n=…` | fMP4 media segment `n` |
| `/nexo/media/player/` | public player page |

A request may be anonymous (a bucket readable by everyone), signed in its headers or presigned. The
URIs of a presigned master playlist are presigned in turn with the same credentials, until the same
expiry: any presigned master playlist (`nexoctl share download --raw` of the master path, an SDK) plays
in a player without credentials. `rev` is the ETag of the object: the URIs answer `412` once it changes.

The console serves the same presentation to its users under
`/api/v1/buckets/{bucket}/objects/hls/` with their session ([Admin API](reference/admin-api.md)).

The video streams count against the [rate limits](configuration.md#rate-limits) of the S3 API, unlike the other
`/nexo/` paths (health probes, metrics).
