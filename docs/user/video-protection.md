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

Two levels:

- **standard** (default): any HLS player plays a protected video (Safari/iOS natively, hls.js, Video.js, ExoPlayer,
  VLC); the token travels in the URLs of the playlist;
- **advanced**: only the Nexo player plays it (its page, `<nexo-player>` and `createPlayer()` of the SDK). The keys
  travel on a session channel (WebSocket) set up with a key exchange, every viewer gets segments encrypted for them,
  and the keys never appear in the network tab ([§5](#5-advanced-protection-the-nexo-player-only)).

On top of either, the **forensic watermark** hides in the picture an invisible code of the playback token: a leaked
copy, even re-encoded, names the viewer it was streamed to ([§6](#6-forensic-watermark)).

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
| Playback | `--mode` | `standard` | `standard`: any HLS player; `advanced`: the Nexo player only |
| Older devices | `--legacy-devices` | `refuse` | advanced mode: `standard` lets the browsers without Media Source Extensions (older iPhones) play the standard way |
| Forensic mark | `--forensic` | off | invisible watermark naming the token of a leaked copy (needs the media worker) |

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

## 5. Advanced protection: the Nexo player only

```sh
nexoctl media protection set local/lms --mode advanced
```

Console: *Video protection* → *Playback* → **Nexo player only**. Nothing changes in your application: it issues the
same tokens and embeds the Nexo player (iframe, `<nexo-player>`, `createPlayer()`) with the same `playlist` URL. What
changes on the wire:

1. The token alone opens nothing: the playlists answer 403 (`X-Nexo-Playback: session`) to every other player.
2. The Nexo player opens a **session** on `wss://…/nexo/media/v1/{bucket}/session` and runs a key exchange (ECDH
   P-256, HKDF-SHA256) whose private key WebCrypto never lets the page read. The server answers with a ticket for
   this session only.
3. Every segment is encrypted **for that session** (AES-128-GCM): a segment captured from one viewer is noise for any
   other, and the playlists name no key a downloader could fetch.
4. The player asks the keys on the channel; they arrive wrapped with the session key and are imported into WebCrypto
   as non-extractable: they never appear in clear in the network tab nor in a JavaScript variable.
5. Revocation, a newer stream of the viewer (max sessions) and the expiry of the token close the channel at once;
   so does the stop of a node (the player reconnects to another one).

**Older devices.** The Nexo player decrypts in a Media Source Extensions loader: Chrome, Edge, Firefox, Safari on
macOS and iPadOS, Safari on iPhone from iOS 17.1. With `--legacy-devices standard` the other browsers play the
standard way; with `refuse` (default) they show an error.

**Proxies.** The session prefers a WebSocket. nginx needs the `Upgrade` headers on `/nexo/media/v1/*/session` (the
[configuration in `deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy/nginx.conf) has them);
Caddy, Traefik, HAProxy and Cloudflare carry WebSockets as they are. When a proxy drops it, the player falls back to
plain `POST` requests on the same path: it still plays.

## 6. Forensic watermark

The moving watermark can be cropped or blurred. The forensic watermark cannot be seen and survives re-encoding,
scaling and screen recording at a decent quality:

```sh
nexoctl media protection set local/lms --forensic
nexoctl media reprocess local/lms/courses/intro.mp4    # each video uploaded before
```

1. The [media worker](media-worker.md) encodes every rendition of a marked video **twice** (variant A and B,
   differing by an invisible pattern of ±2 luma levels): twice the encoding time and the storage of the renditions.
2. Every playback token has a secret 64-bit code; segment *n* of its streams comes from variant A or B according to
   bit *n mod 64*. A copy of a few minutes carries the whole code.
3. At issue, Nexo records the token (subject, watermark, session, times) in the bucket, encrypted, under
   `.nexo-media/.forensic/`. A marked video never streams before its marked renditions exist (503 meanwhile), and
   never from the original.

**Tracing a leak.** Download the copy (convert it to an MP4 with H.264 video if needed), then:

```sh
nexo-media-worker trace --leak copy.mp4 --bucket lms --key courses/intro.mp4 --out votes.json
nexoctl media trace local/lms/courses/intro.mp4 --votes votes.json
```

```text
Code read from the copy of `local/lms/courses/intro.mp4`: 0110…1 (64 bits known)
#    SUBJECT                  SESSION                          AGREE     ISSUED
1    student-1842             9f2c…                            63/64     2026-03-02T18:04:11Z  (mario.rossi@example.com)
```

The worker compares every few pictures of the copy with the two variants and writes votes; Nexo reads the code and
lists the tokens agreeing with at least 7/8 of its bits (`admin:TracePlaybackLeaks` on the bucket, Admin API
`POST /api/v1/buckets/{bucket}/objects/playback-trace`). A clip that starts later in the video needs `--offset`.
A short clip (few segments) agrees with several tokens: the longer the copy, the fewer the candidates.

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
- The design, the protocol of the sessions and the forensic watermark are described in the
  [proposal](https://github.com/R3n3r0/Nexo/blob/main/docs/proposals/video-protection.md) (§10, §11).

## What it does not do

It is not commercial DRM (Widevine, FairPlay, PlayReady): a browser cannot keep a secret from its user. A determined
user can record the screen or, in the advanced mode, hook the decoder of the page, at playback speed, for the video
and the session they are watching. The protection makes bulk download useless, bounds sharing, and the watermarks
trace a leak back to the account. The forensic watermark is weakened by viewers colluding (averaging copies from
several accounts), by heavy cropping or geometric transforms, and by very short clips.
