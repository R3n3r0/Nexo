# Video protection

Protect the videos of a bucket against download — for a course platform (LMS), a members' area, an intranet — without
any DRM vendor or license server: everything runs in Nexo. A protected video:

- **streams encrypted**: every HLS segment is encrypted with AES-128 (HLS `METHOD=AES-128`), a new key every 10
  segments (about a minute). A downloader (yt-dlp, ffmpeg, browser extensions) saves noise without the keys;
- **gets its keys only with a playback token** of that video, issued by your application for a viewer, at most
  *pace* times faster than the video plays, and to at most *max sessions* simultaneous streams of the viewer (a newer
  stream replaces the oldest: the account shared with a friend keeps kicking one of them out);
- **cannot be downloaded as a file**: `GetObject`, copies, `SelectObjectContent`, the console download, preview and
  ZIP and the distributions refuse the original to everyone without `nexo:DownloadProtectedMedia`;
- **shows the viewer's watermark** (a name, an e-mail) moving over the picture in the Nexo player and in the
  [JavaScript SDK](https://github.com/R3n3r0/Nexo/tree/main/sdk/js), also in full screen.

Any HLS player plays a protected video (Safari/iOS natively, hls.js, Video.js, ExoPlayer, VLC): the token travels
in the URLs of the playlist.

## 1. Turn it on

Console: bucket → **Media** → *Video protection*. Or:

```sh
nexoctl media protection set local/lms --enable --prefix courses/
nexoctl media protection get local/lms
```

| Setting | Flag | Default | Meaning |
| --- | --- | --- | --- |
| Enabled | `--enable` / `--disable` | off | |
| Prefix | `--prefix` | whole bucket | only the keys under it |
| Suffixes | `--suffix` | `.mp4`, `.mov`, `.m4v` | the protected videos (their renditions too) |
| Token TTL | `--token-ttl` | 2h (max 24h) | longest validity of a token |
| Max sessions | `--max-sessions` | 2 (`unlimited`) | simultaneous streams per viewer |
| Pace | `--pace` | 2 | keys at most this many times faster than playback (after a burst of 3 key periods) |
| Watermark | `--watermark` | on | the Nexo player draws the viewer |
| Allowed origins | `--origin` | any | web origins allowed to fetch the keys (`https://learn.example.com`) |
| Share links | `--share-links` | off | stream share links (presigned) also play protected videos |

The media worker reads the originals to derive renditions: its policy needs `nexo:DownloadProtectedMedia` (the
policy of the [media worker guide](media-worker.md) includes it).

## 2. Issue playback tokens from your application

Your backend — never the browser — calls the Admin API with an access key allowed `nexo:IssuePlaybackToken` on the
videos (and `admin:RevokePlaybackTokens` on the bucket to revoke):

```sh
nexoctl media token local/lms/courses/intro.mp4 --subject student-1842 --watermark "mario.rossi@example.com"
```

```http
POST /api/v1/buckets/lms/objects/playback-tokens?key=courses/intro.mp4      (SigV4, Admin API listener)
{"subject": "student-1842", "watermark": "mario.rossi@example.com", "ttlSeconds": 7200}

201 {"token": "...", "sessionId": "9f2c...", "expiresAt": "...",
     "playlist": "https://nexo.example.com:9000/nexo/media/v1/lms/master.m3u8?key=courses%2Fintro.mp4&pt=...",
     "player":   "https://nexo.example.com:9000/nexo/media/player/#/nexo/media/v1/lms/master.m3u8?..."}
```

The [JavaScript SDK](https://github.com/R3n3r0/Nexo/tree/main/sdk/js) wraps it for Node.js
(`createNexoClient().issuePlaybackToken()`); any language with a SigV4 signer works, and so does the Go package
`nexoadmin` (`IssuePlaybackToken`).

The `subject` identifies the viewer for your application (an account id): the simultaneous streams are counted by
subject. The token is readable (not secret) and carries the watermark text; only Nexo can sign it.

## 3. Play

- **iframe**: `<iframe src="{player}" allow="fullscreen">` — the Nexo player page with the watermark;
- **your page**: `<nexo-player src="{playlist}">` of the SDK (served by Nexo at `/nexo/media/sdk/index.js`), or
  `createPlayer()` in React, Angular, Vue (see the [SDK](https://github.com/R3n3r0/Nexo/tree/main/sdk/js));
- **any HLS player** with `{playlist}` (apps: AVPlayer, ExoPlayer).

The playlists, segments and keys are on the S3 listener; its CORS policy (`api.cors_allow_origin`, any origin by
default) lets pages of other origins fetch them.

## 4. Revoke

```sh
nexoctl media revoke local/lms --session 9f2c...        # one token
nexoctl media revoke local/lms --subject student-1842   # every token of a viewer issued so far
```

A revoked token stops on every node within about 10 seconds. A token issued afterwards plays.

## How it works

- The keys derive from a key of the deployment (itself derived from the root credentials and the deployment ID),
  the bucket, the key, the version and the ETag: nothing is stored per video, overwriting a video changes its keys,
  every node computes the same keys. **Changing the root credentials invalidates every token and every key.**
- Revocations and the streams of the viewers are kept in the system documents, shared by the nodes; the pace is
  measured on each node.
- The owners keep watching their protected videos in the console (its session gets the keys).
- Distributions refuse the original of a protected video and its playlists: protected videos play on the S3
  listener, with a token.
- `nexo_media_keys_total{result="ok|sessions|pace"}` counts the keys served and refused.

## What it does not do

It is not commercial DRM (Widevine, FairPlay, PlayReady): a determined user can record the screen or extract the
keys of the stream they are watching, at playback speed. The protection makes bulk download useless, bounds sharing,
and the moving watermark traces a leak back to the account.
