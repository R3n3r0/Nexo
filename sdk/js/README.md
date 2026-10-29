# Nexo player SDK (JavaScript)

Play the videos of [Nexo](https://github.com/R3n3r0/Nexo) — protected or not — in any web page, and issue their
playback tokens from your backend. No DRM vendor, no license server: everything runs in Nexo.

*[Leggi in italiano](README.it.md)*

## Do I need a special player?

No. A protected video is standard HLS with `METHOD=AES-128` encryption: **any HLS player plays it** (Safari and iOS
natively, hls.js, Video.js, Shaka, ExoPlayer, AVPlayer, VLC) as long as it receives the playlist URL with its
playback token. The token travels in every URL of the playlist, so the player needs no credentials and no custom
code.

This SDK only makes the common case short:

| Part | What it does | Where it runs |
| --- | --- | --- |
| `<nexo-player>` / `createPlayer()` | Plays the playlist (native HLS or hls.js), draws the viewer's **moving watermark** over the picture (also in full screen), reports expired or revoked tokens | Browser: plain HTML, React, Angular, Vue, Svelte... |
| `createNexoClient()` (`@nexo-storage/player/server`) | Issues and revokes playback tokens with the Admin API (SigV4-signed, no dependency) | Your backend: Node.js 18+, Deno, Bun |

The access key **never** reaches the browser: your backend issues a token for the logged-in user and hands the page
only the playlist URL.

```
 Browser (your LMS page)          Your backend                     Nexo
 ───────────────────────          ────────────                     ────
 GET /lesson/42  ───────────────► check the user can watch
                                  issuePlaybackToken() ──────────► POST /api/v1/buckets/lms/objects/playback-tokens
                 ◄─────────────── page with playlist URL ◄──────── { playlist, player, sessionId, expiresAt }
 <nexo-player src=playlist> ─────────────────────────────────────► playlists, encrypted segments, keys (?pt=token)
```

## 1. Protect the videos (once)

Console → bucket → **Media** → *Video protection*, or:

```sh
nexoctl media protection set local/lms --enable --prefix courses/ --max-sessions 2 --origin https://learn.example.com
```

Create the access key of your backend with a policy that allows only the tokens:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["nexo:IssuePlaybackToken"], "Resource": ["arn:aws:s3:::lms/courses/*"]},
    {"Effect": "Allow", "Action": ["admin:RevokePlaybackTokens"], "Resource": ["arn:aws:s3:::lms"]}
  ]
}
```

## 2. Issue a token in your backend

```js
// npm install @nexo-storage/player   (or copy src/server.js and src/sigv4.js)
import { createNexoClient } from '@nexo-storage/player/server';

const nexo = createNexoClient({
  endpoint: 'https://nexo.example.com:9001', // the Admin API listener (console address)
  accessKey: process.env.NEXO_ACCESS_KEY,
  secretKey: process.env.NEXO_SECRET_KEY,
  region: 'us-east-1',
});

// Express: the page of a lesson
app.get('/lesson/:id', requireLogin, async (req, res) => {
  const lesson = await db.lessons.find(req.params.id);
  if (!(await canWatch(req.user, lesson))) return res.sendStatus(403);
  const { playlist } = await nexo.issuePlaybackToken({
    bucket: 'lms',
    key: lesson.videoKey,                 // courses/intro.mp4
    subject: String(req.user.id),         // counts the simultaneous streams of this user
    watermark: req.user.email,            // drawn over the video
    ttlSeconds: 2 * 3600,
  });
  res.render('lesson', { playlist });
});

// When a subscription ends, or a user shares the account:
await nexo.revokePlaybackSubject('lms', String(user.id));
```

Other languages: the token API is one signed HTTP call,
`POST /api/v1/buckets/{bucket}/objects/playback-tokens?key=...` with `{"subject": "...", "watermark": "...",
"ttlSeconds": 7200}`, signed with AWS Signature V4 (service `s3`), like any S3 request: use the SigV4 signer of the
AWS SDK of your language (boto3/botocore `SigV4Auth`, PHP `Aws\Signature\SignatureV4`, Java `Aws4Signer`,
`aws-sigv4` in Rust, `github.com/aws/aws-sdk-go-v2/aws/signer/v4` or the `nexoadmin` Go package), or
`nexoctl media token` in a script.

## 3. Play it in the page

### Plain HTML (no build)

Nexo serves the SDK itself:

```html
<script src="https://cdn.jsdelivr.net/npm/hls.js@1"></script>
<script type="module">
  import { defineNexoPlayer } from 'https://nexo.example.com:9000/nexo/media/sdk/index.js';
  defineNexoPlayer();
</script>

<nexo-player src="{{ playlist }}" style="max-width: 960px"></nexo-player>
```

Attributes: `src` (the playlist), `watermark` (text, or `off`; default the one of the token), `autoplay`, `muted`,
`poster`. The element fires `nexo-error` with `{code, status, message}`; `element.player.video` is the `<video>`.

Or, without any script, an iframe with the Nexo player page (`player` in the token answer):

```html
<iframe src="{{ player }}" allow="fullscreen; autoplay" style="width: 100%; aspect-ratio: 16/9; border: 0"></iframe>
```

### React

```jsx
import Hls from 'hls.js';
import { useEffect, useRef } from 'react';
import { createPlayer } from '@nexo-storage/player';

export function LessonVideo({ playlist }) {
  const box = useRef(null);
  useEffect(() => {
    const player = createPlayer(box.current, { src: playlist, hls: Hls, onError: (e) => console.warn(e.message) });
    return () => player.destroy();
  }, [playlist]);
  return <div ref={box} style={{ aspectRatio: '16 / 9' }} />;
}
```

### Angular

```ts
import { Component, ElementRef, Input, OnChanges, OnDestroy, ViewChild } from '@angular/core';
import Hls from 'hls.js';
import { createPlayer, type Player } from '@nexo-storage/player';

@Component({
  selector: 'app-lesson-video',
  standalone: true,
  template: `<div #box style="aspect-ratio: 16/9"></div>`,
})
export class LessonVideoComponent implements OnChanges, OnDestroy {
  @Input({ required: true }) playlist!: string;
  @ViewChild('box', { static: true }) box!: ElementRef<HTMLDivElement>;
  private player?: Player;

  ngOnChanges() {
    this.player?.destroy();
    this.player = createPlayer(this.box.nativeElement, { src: this.playlist, hls: Hls });
  }

  ngOnDestroy() {
    this.player?.destroy();
  }
}
```

(Or `defineNexoPlayer(Hls)` once and `<nexo-player [attr.src]="playlist">` with `CUSTOM_ELEMENTS_SCHEMA`.)

### Vue

```vue
<script setup>
import Hls from 'hls.js';
import { onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { createPlayer } from '@nexo-storage/player';

const props = defineProps({ playlist: String });
const box = ref(null);
let player;
const start = () => { player?.destroy(); player = createPlayer(box.value, { src: props.playlist, hls: Hls }); };
onMounted(start);
watch(() => props.playlist, start);
onBeforeUnmount(() => player?.destroy());
</script>

<template><div ref="box" style="aspect-ratio: 16/9" /></template>
```

### Any other HLS player

```js
const hls = new Hls();
hls.loadSource(playlist); // the token is already in the URL: nothing else to configure
hls.attachMedia(video);
```

Draw your own watermark if you like: `watermarkText(playlistClaims(playlist))` gives the text of the token.

## Errors

| `code` | HTTP | Meaning | What to do |
| --- | --- | --- | --- |
| `denied` | 403 | Token expired or revoked, or a newer stream of the same viewer replaced this one (`maxSessions`), or the page's origin is not allowed | Ask your backend for a new token; tell the user that the video plays elsewhere |
| `too-fast` | 429 | Keys asked faster than the video plays (a downloader) | Wait |
| `not-found` | 404 | No such video | |

To renew a token without reloading the page: `player.load(newPlaylist)`.

## What the protection does — and does not do

The segments are encrypted (AES-128, a key every 10 segments); the keys go only to a valid token, at the pace of
playback, to at most `maxSessions` simultaneous streams of a viewer; the original file cannot be downloaded without
`nexo:DownloadProtectedMedia`. A downloader saves noise; a shared link expires, stops when revoked, and shows the
name of whoever shared it. As with every non-commercial-DRM scheme, a determined user can still record the screen:
the moving watermark identifies the account it came from.

## Development

```sh
cd sdk/js && npm test   # node --test: SigV4 against the AWS reference vector, the token API, the claims
```

License: AGPL-3.0-only, like Nexo. Contact: <paolorusso1991@gmail.com>.
