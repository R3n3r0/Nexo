// A player of the HLS presentations of Nexo for any web page: native HLS
// (Safari, iOS) or hls.js (Media Source Extensions), the AES-128 keys of
// the protected videos fetched with the playback token of the playlist,
// and the viewer's watermark moving over the picture, also in full screen.
// A video that plays only in the player of Nexo (the advanced playback)
// plays in a session (session.js): keys negotiated over a WebSocket,
// segments encrypted for the session and decrypted before hls.js sees
// them.

import { openSession, sessionLoader } from './session.js';
import { playlistClaims, watermarkText } from './token.js';

const styleId = 'nexo-player-style';
const css = `
.nexo-player{position:relative;display:flex;align-items:center;justify-content:center;background:#000;overflow:hidden}
.nexo-player video{width:100%;max-height:100%;display:block}
.nexo-player .nexo-watermark{position:absolute;z-index:1;max-width:60%;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;
color:rgba(255,255,255,.35);font:clamp(.75rem,2.2vw,1.25rem) system-ui,sans-serif;text-shadow:0 0 2px rgba(0,0,0,.6);
pointer-events:none;user-select:none;transition:left 1.5s ease,top 1.5s ease}
.nexo-player .nexo-message{position:absolute;inset:auto 0;margin:0;padding:1rem;color:#ddd;text-align:center;font:1rem system-ui,sans-serif}
`;

/**
 * Installs the stylesheet of the player in root (a document or a shadow root), once.
 * @param {Document | ShadowRoot} root
 */
export function installStyles(root) {
  if (root.getElementById?.(styleId) || root.querySelector?.(`#${styleId}`)) return;
  const style = document.createElement('style');
  style.id = styleId;
  style.textContent = css;
  (root instanceof Document ? root.head : root).appendChild(style);
}

/**
 * Plays a playlist of Nexo in container (a block element sized by the page).
 * @param {HTMLElement} container
 * @param {import('./index.js').PlayerOptions} options
 * @returns {import('./index.js').Player}
 */
export function createPlayer(container, options) {
  const root = container.getRootNode();
  installStyles(root instanceof ShadowRoot ? root : document);
  container.classList.add('nexo-player');
  const video = document.createElement('video');
  video.controls = options.controls ?? true;
  video.playsInline = true;
  video.autoplay = Boolean(options.autoplay);
  video.muted = Boolean(options.muted);
  if (options.poster) video.poster = options.poster;
  // Not a protection (a downloader ignores them), a hint to the browser UI.
  video.setAttribute('controlslist', 'nodownload');
  video.disablePictureInPicture = true;
  video.addEventListener('contextmenu', (e) => e.preventDefault());
  container.appendChild(video);

  const message = document.createElement('p');
  message.className = 'nexo-message';
  message.hidden = true;
  container.appendChild(message);

  /** @type {any} */
  let hls;
  /** @type {import('./index.js').Session | undefined} */
  let session;
  let generation = 0;
  let stopMark = () => {};
  const fail = (/** @type {import('./index.js').PlayerError} */ err) => {
    if (options.onError) options.onError(err);
    else {
      message.textContent = err.message;
      message.hidden = false;
    }
  };

  // Full screen: the container (with the watermark) instead of the bare video.
  const onFullscreen = () => {
    if (document.fullscreenElement === video) {
      document.exitFullscreen().then(() => container.requestFullscreen()).catch(() => {});
    }
  };
  document.addEventListener('fullscreenchange', onFullscreen);

  const stop = () => {
    generation++;
    if (hls) {
      hls.destroy();
      hls = undefined;
    }
    if (session) {
      session.close();
      session = undefined;
    }
  };

  // Plays a playlist with hls.js; extra is merged in its configuration.
  const playHls = (/** @type {any} */ Hls, /** @type {string} */ src, /** @type {Record<string, unknown>} */ extra, /** @type {(status: number) => boolean} */ retry) => {
    hls = new Hls({ ...(options.hlsConfig ?? {}), ...extra });
    hls.on(Hls.Events.ERROR, (/** @type {any} */ _event, /** @type {any} */ data) => {
      if (!data.fatal) return;
      const status = data.response?.code ?? 0;
      hls.destroy();
      hls = undefined;
      if (!retry(status)) fail(playerError(status, data.details));
    });
    hls.loadSource(src);
    hls.attachMedia(video);
  };

  // Plays a video of the advanced playback: a session, then hls.js with
  // the loader that decrypts its segments.
  const playSession = (/** @type {any} */ Hls, /** @type {string} */ src) => {
    const mine = ++generation;
    openSession(src, {
      onClosed: (err) => {
        if (mine !== generation) return;
        stop();
        fail(err);
      },
    }).then(
      (s) => {
        if (mine !== generation) {
          s.close();
          return;
        }
        session = s;
        playHls(Hls, s.master, { fLoader: sessionLoader(Hls, s) }, () => false);
      },
      (err) => {
        if (mine === generation) fail(err);
      },
    );
  };

  const load = (/** @type {string} */ src) => {
    stopMark();
    message.hidden = true;
    stop();
    const claims = playlistClaims(src);
    const text = options.watermark === false ? undefined : (options.watermark ?? watermarkText(claims));
    stopMark = text ? startWatermark(container, text, options.watermarkInterval) : () => {};

    const Hls = options.hls ?? globalThis.Hls;
    const mse = Boolean(Hls && Hls.isSupported());
    if (mse && claims?.m === 'session') {
      playSession(Hls, src);
    } else if (claims?.m === 'session' && video.canPlayType('application/vnd.apple.mpegurl') !== '') {
      // No Media Source Extensions (an older iPhone): the standard way,
      // when the protection lets these devices take it.
      fetch(src).then(
        (resp) => {
          if (resp.ok) video.src = src;
          else if (resp.status === 403) fail({ code: 'unsupported', status: 403, message: 'This video plays only in the Nexo player, which this browser cannot run: update it or use another browser.' });
          else fail(playerError(resp.status, 'manifestLoadError'));
        },
        () => fail(playerError(0, 'manifestLoadError')),
      );
    } else if (mse) {
      // A token issued before the video moved to the advanced playback:
      // refused the standard way, it plays in a session.
      let retried = false;
      playHls(Hls, src, {}, (status) => {
        if (status !== 403 || retried || !claims) return false;
        retried = true;
        playSession(Hls, src);
        return true;
      });
    } else if (video.canPlayType('application/vnd.apple.mpegurl') !== '') {
      video.src = src;
      video.addEventListener('error', () => fail(playerError(0, 'mediaError')), { once: true });
    } else {
      fail({ code: 'unsupported', status: 0, message: 'This browser cannot play HLS: load hls.js (https://github.com/video-dev/hls.js).' });
    }
  };
  load(options.src);

  return {
    video,
    load,
    destroy() {
      stopMark();
      document.removeEventListener('fullscreenchange', onFullscreen);
      stop();
      video.removeAttribute('src');
      video.load();
      video.remove();
      message.remove();
      container.classList.remove('nexo-player');
    },
  };
}

/**
 * The error of a failed request of the player.
 * @param {number} status
 * @param {string} details
 * @returns {import('./index.js').PlayerError}
 */
export function playerError(status, details) {
  switch (status) {
    case 401:
    case 403:
      return { code: 'denied', status, message: 'The video cannot be played: the link has expired, was revoked, or is used by a newer stream of the same viewer.' };
    case 404:
      return { code: 'not-found', status, message: 'The video does not exist.' };
    case 429:
      return { code: 'too-fast', status, message: 'The video is read faster than it plays: try again in a minute.' };
    default:
      return { code: 'failed', status, message: `The video cannot be played (${details}).` };
  }
}

/**
 * Draws text over stage, moving it every few seconds; returns the function
 * that removes it.
 * @param {HTMLElement} stage
 * @param {string} text
 * @param {number} [everyMs]
 */
export function startWatermark(stage, text, everyMs = 5000) {
  const mark = document.createElement('div');
  mark.className = 'nexo-watermark';
  mark.textContent = text;
  mark.setAttribute('aria-hidden', 'true');
  stage.appendChild(mark);
  const move = () => {
    mark.style.left = `${Math.round(5 + Math.random() * 60)}%`;
    mark.style.top = `${Math.round(5 + Math.random() * 80)}%`;
  };
  move();
  const timer = setInterval(move, everyMs);
  return () => {
    clearInterval(timer);
    mark.remove();
  };
}
