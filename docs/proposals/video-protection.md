# Proposal: protected video streaming (download protection)

Status: **implemented** (user guide: [docs/user/video-protection.md](../user/video-protection.md)).

## 1. Summary

A bucket can mark its videos as **protected**, from the console or the API. A protected video:

1. **streams encrypted**: every HLS segment is encrypted with AES-128 (HLS `METHOD=AES-128`), with a key that
   changes every few segments. A file saved by a downloader (yt-dlp, ffmpeg, a browser extension) is noise
   without the keys;
2. **gets its keys only through Nexo**, from a key endpoint that answers a **playback token** for that video (or
   a console session or a signed request of someone allowed to read the object), never faster than the video
   plays, and to a bounded number of simultaneous viewers per user;
3. **cannot be downloaded as a file**: `GetObject` (and every copy or read of the original) is refused to everyone
   but the identities allowed `nexo:DownloadProtectedMedia`;
4. **shows a watermark** of the viewer (a name, an e-mail, an id chosen by the application) moving over the picture
   in the Nexo player, which deters screen recordings and traces a leak back to an account.

Everything runs inside `nexo`: no DRM vendor, no license server, no external service. Any application (a course
platform, an intranet, a members' area) issues playback tokens for its users with an access key, through the Admin
API, `nexoctl` or any HTTP client, and embeds the Nexo player or any HLS player.

## 2. What it protects against, and what it does not

| Threat | Protection |
| --- | --- |
| Download of the original file (S3 API, console, share link, copy to another bucket) | Refused: `nexo:DownloadProtectedMedia` only |
| Saving the HLS stream with a downloader | The segments are encrypted; the keys come only with a valid token, at most at playback pace: a bulk download stops after a few keys |
| Sharing a playback link | The token expires (default 2 h), names its viewer (watermark), counts against the viewer's simultaneous streams and can be revoked |
| Many people on one account | Simultaneous streams per viewer (default 2) |
| Screen recording | Not preventable in a browser without commercial DRM: the moving watermark identifies the account that recorded |
| A skilled user extracting the keys from the browser while watching | Possible (as with every key-in-the-browser scheme): limited to the pace of playback and to one video per token, traced by the watermark and the audit log |

Commercial DRM (Widevine, FairPlay, PlayReady) needs licenses and key servers from Google, Apple or Microsoft and is
not part of this proposal; the AES-128 packaging keeps the door open (the same segments could later be packaged as
CENC `cbcs`).

## 3. Configuration

Per bucket, `PUT /api/v1/buckets/{bucket}/media/protection` (`admin:SetBucketMedia`), the console (bucket →
*Media* → *Video protection*) and `nexoctl media protection set`:

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | false | Protection on |
| `prefix` | "" | Only the keys under the prefix (empty: every video of the bucket) |
| `tokenTTL` | 2h | Longest validity of a playback token (at most 24 h) |
| `maxSessions` | 2 | Simultaneous streams per viewer (0: no limit) |
| `paceFactor` | 2 | Keys are served at most this many times faster than the video plays (plus a burst) |
| `watermark` | true | The Nexo player draws the viewer's watermark |
| `allowedOrigins` | [] | Web origins allowed to embed the player and fetch the keys (empty: any) |
| `shareLinks` | false | Stream share links (managed shares) play protected videos too (without watermark nor limits) |

A video is protected when its bucket's protection is enabled and its key matches the prefix; MP4 videos only (the
suffixes of the media rules).

## 4. Playback tokens

`POST /api/v1/buckets/{bucket}/objects/playback-tokens?key=courses/intro.mp4` (policy action
`nexo:IssuePlaybackToken` on the object), `nexoctl media token`:

```json
{ "subject": "student-1842", "watermark": "mario.rossi@example.com", "ttlSeconds": 7200 }
```

answers the token, its expiry and two URLs: the master playlist for any HLS player and the Nexo player page.

The token is `base64url(payload).base64url(HMAC-SHA256)`: the payload names the bucket, the key, the version, the
subject, the watermark text, a random session id, the issue and expiry times. It is signed with a key derived from
the deployment's **media protection key** (§6), so every node verifies it alone. It is a capability for one video:
it grants nothing else, needs no credentials in the player, and is carried by every URI of the playlists (`pt=`).

Revocation (`admin:RevokePlaybackTokens` on the bucket): `DELETE /api/v1/buckets/{bucket}/media/playback-sessions/{sid}`
revokes one token, `DELETE /api/v1/buckets/{bucket}/media/playback-subjects/{subject}` every token of a viewer of the
bucket issued so far.

## 5. Packaging

- The init segment stays clear (it holds the codec configuration, no picture): `EXT-X-MAP` precedes the first
  `EXT-X-KEY`.
- Key period: 10 segments (about a minute). Segment *n* is encrypted with AES-128-CBC (PKCS#7 padding) under key
  *n/10*, with the media sequence number as IV (RFC 8216 §5.2): one `EXT-X-KEY:METHOD=AES-128,URI="key?…&i=<period>"`
  per period.
- Key of a period: the first 16 bytes of `HMAC-SHA256(media key, "nexo/media/aes128" ‖ bucket ‖ key ‖ version ‖
  ETag ‖ period)`, the key being that of the rendition file for a rendition. Nothing is stored per video; overwriting a video changes its keys; renditions have
  their own keys.
- Plays natively in Safari/iOS, in hls.js (Chrome, Firefox, Edge, the Nexo player and console), ExoPlayer, VLC,
  Video.js.

## 6. Media protection key

Derived, like the console session key, from the root credentials and the deployment ID (`HMAC-SHA256(root
password, "nexo-media-protection-v1" ‖ root user ‖ deployment ID)`): every node derives the same key, no other
deployment does, nothing is stored nor replicated. Changing the root credentials rotates it, which invalidates every
token and every segment key at once.

## 7. Key endpoint

`GET /nexo/media/v1/{bucket}/key?key=…&rev=…&i=…&pt=…` (and the console's `…/objects/hls/key`): 16 bytes,
`Cache-Control: no-store`. It answers when:

1. the token is valid for that object, not expired nor revoked; or the request is a console session or signed
   (not presigned) request of an identity allowed `s3:GetObject` (owners previewing their videos); or a stream
   share link when `shareLinks` is on;
2. the `Origin` (when present) is one of `allowedOrigins`;
3. the token's session is one of the subject's `maxSessions` streams for the bucket: a new stream beyond the limit
   replaces the oldest one, which gets no key afterwards (a reload of the page plays at once; an account used by two
   people keeps interrupting one of them);
4. the number of distinct periods the session fetched stays under `elapsed / period × paceFactor + burst` (burst 3):
   a player seeking around is fine, a downloader asking for every key at once stops after the burst (429).

The streams of the viewers and the revocations live in the system store (shared by the nodes, cached for 10 s); the
pace is measured on each node. The checks are counted in `nexo_media_keys_total{result}`. The distributions refuse
the protected videos (their original and their playlists).

## 8. Original download

`GetObject`, `HeadObject` with `partNumber`, `CopyObject`/`UploadPartCopy` sources, `SelectObjectContent`, the console
download and ZIP export of a protected key need `nexo:DownloadProtectedMedia` (root and the owner's policies may
grant it). `nexo:GetMediaStream` stays the right to stream.

## 9. Player and watermark

The Nexo player page reads the watermark from the token's payload and draws it over the video, moving every few
seconds, semi-transparent; it is also drawn in full screen. The page is embeddable in an `<iframe>` from the
`allowedOrigins` (`frame-ancestors`). Applications using their own player can draw their own watermark.

## 10. Work items

1. Domain and bucket configuration, Admin API, OpenAPI, `nexoadmin`, CLI, console section.
2. Media protection key, AES-128 packaging, key endpoint (S3 listener and console).
3. Playback tokens: issuance, verification on the media routes, sessions, pace, revocation.
4. Original download refused.
5. Player watermark and embedding; distributions' video behavior with tokens.
6. Documentation (EN/IT), CHANGELOG, tests (unit, integration in every mode, e2e).
