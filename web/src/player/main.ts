import './player.css';

import Hls, { type FragmentLoaderConstructor } from 'hls.js';

import { openSession, sessionLoader } from '../../../sdk/js/src/session.js';
import { masterUrl } from './source';
import { playbackClaims, startWatermark, watermarkText } from './watermark';

/**
 * The public player of the stream shares, served by the S3 listener with
 * the playlists: it plays the master playlist of its fragment natively
 * (Safari) or with hls.js (Media Source Extensions).
 */
const video = document.querySelector<HTMLVideoElement>('#video');
const status = document.querySelector<HTMLParagraphElement>('#status');

function fail(message: string) {
  if (status) {
    status.textContent = message;
    status.hidden = false;
  }
}

const src = masterUrl(window.location);
const stage = document.querySelector<HTMLElement>('#stage');
const claims = src ? playbackClaims(src) : undefined;
const mark = watermarkText(claims);
if (stage && video && mark) {
  // A playback token names its viewer: drawn over the picture, also in
  // full screen (the stage goes full screen instead of the bare video).
  startWatermark(stage, mark);
  document.addEventListener('fullscreenchange', () => {
    if (document.fullscreenElement === video) {
      void document.exitFullscreen().then(() => stage.requestFullscreen());
    }
  });
}
if (!video || !src) {
  fail('This link is not a video link.');
} else if (claims?.m === 'session' && Hls.isSupported()) {
  // The video plays only in a session of the Nexo player: keys negotiated
  // over a WebSocket, segments decrypted before hls.js sees them.
  let hls: Hls | undefined;
  openSession(src, {
    onClosed: (err) => {
      hls?.destroy();
      fail(`The video stopped: ${err.message}`);
    },
  }).then(
    (session) => {
      hls = new Hls({ fLoader: sessionLoader(Hls, session) as FragmentLoaderConstructor });
      hls.on(Hls.Events.ERROR, (_event, data) => {
        if (!data.fatal) return;
        hls?.destroy();
        fail('The video cannot be played: the link may have expired or been revoked.');
      });
      hls.loadSource(session.master);
      hls.attachMedia(video);
    },
    (err: unknown) => {
      const reason = (err as { message?: unknown } | undefined)?.message;
      fail(
        `The video cannot be played: ${typeof reason === 'string' ? reason : 'the session failed'}.`,
      );
    },
  );
} else if (video.canPlayType('application/vnd.apple.mpegurl') !== '') {
  video.src = src.toString();
  video.addEventListener('error', () => {
    fail('The video cannot be played: the link may have expired or been revoked.');
  });
} else if (Hls.isSupported()) {
  const hls = new Hls();
  hls.on(Hls.Events.ERROR, (_event, data) => {
    if (!data.fatal) return;
    hls.destroy();
    fail(
      data.details === Hls.ErrorDetails.MANIFEST_INCOMPATIBLE_CODECS_ERROR
        ? 'This browser cannot play the format of the video.'
        : 'The video cannot be played: the link may have expired or been revoked.',
    );
  });
  hls.loadSource(src.toString());
  hls.attachMedia(video);
} else {
  fail('This browser cannot play the video.');
}
