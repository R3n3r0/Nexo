// A player of the HLS presentations of Nexo for any web page: native HLS
// (Safari, iOS) or hls.js (Media Source Extensions), the AES-128 keys of
// the protected videos fetched with the playback token of the playlist,
// and the viewer's watermark moving over the picture, also in full screen.

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

  const load = (/** @type {string} */ src) => {
    stopMark();
    message.hidden = true;
    if (hls) {
      hls.destroy();
      hls = undefined;
    }
    const text = options.watermark === false ? undefined : (options.watermark ?? watermarkText(playlistClaims(src)));
    stopMark = text ? startWatermark(container, text, options.watermarkInterval) : () => {};

    const Hls = options.hls ?? globalThis.Hls;
    if (Hls && Hls.isSupported()) {
      hls = new Hls(options.hlsConfig ?? {});
      hls.on(Hls.Events.ERROR, (/** @type {any} */ _event, /** @type {any} */ data) => {
        if (!data.fatal) return;
        const status = data.response?.code ?? 0;
        fail(playerError(status, data.details));
        hls.destroy();
        hls = undefined;
      });
      hls.loadSource(src);
      hls.attachMedia(video);
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
      if (hls) hls.destroy();
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
