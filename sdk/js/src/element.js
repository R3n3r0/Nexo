// <nexo-player src="https://nexo.example.com/nexo/media/v1/...&pt=..."></nexo-player>
// A custom element wrapping createPlayer, for plain HTML and for the
// frameworks that render custom elements (Angular, Vue, Svelte, React 19).

import { createPlayer } from './player.js';

const Base = typeof HTMLElement === 'undefined' ? /** @type {any} */ (class {}) : HTMLElement;

export class NexoPlayerElement extends Base {
  static observedAttributes = ['src', 'watermark'];

  /** The hls.js class used by every element (default: globalThis.Hls). */
  static Hls = undefined;

  /** @type {import('./index.js').Player | undefined} */
  #player;

  connectedCallback() {
    if (!this.shadowRoot) {
      const shadow = this.attachShadow({ mode: 'open' });
      const host = document.createElement('style');
      host.textContent = ':host{display:block;aspect-ratio:16/9;background:#000}:host>div{width:100%;height:100%}';
      shadow.appendChild(host);
      shadow.appendChild(document.createElement('div'));
    }
    this.#start();
  }

  disconnectedCallback() {
    this.#player?.destroy();
    this.#player = undefined;
  }

  attributeChangedCallback() {
    if (this.isConnected) this.#start();
  }

  /** The player of the element (its <video> is player.video). */
  get player() {
    return this.#player;
  }

  #start() {
    const src = this.getAttribute('src');
    this.#player?.destroy();
    this.#player = undefined;
    const stage = this.shadowRoot?.querySelector('div');
    if (!src || !stage) return;
    const mark = this.getAttribute('watermark');
    this.#player = createPlayer(/** @type {HTMLElement} */ (stage), {
      src,
      hls: /** @type {any} */ (this.constructor).Hls,
      watermark: mark === 'off' ? false : (mark ?? undefined),
      autoplay: this.hasAttribute('autoplay'),
      muted: this.hasAttribute('muted'),
      poster: this.getAttribute('poster') ?? undefined,
      onError: (err) => {
        this.dispatchEvent(new CustomEvent('nexo-error', { detail: err, bubbles: true, composed: true }));
        const p = document.createElement('p');
        p.className = 'nexo-message';
        p.textContent = err.message;
        stage.appendChild(p);
      },
    });
  }
}

/**
 * Registers <nexo-player> (once); hls is the hls.js class for the browsers
 * without native HLS (default: globalThis.Hls).
 * @param {any} [hls]
 */
export function defineNexoPlayer(hls) {
  if (hls) NexoPlayerElement.Hls = hls;
  if (typeof customElements !== 'undefined' && !customElements.get('nexo-player')) {
    customElements.define('nexo-player', NexoPlayerElement);
  }
}
