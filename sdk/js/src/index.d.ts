/** Claims of a playback token (readable, not secret). */
export interface PlaybackClaims {
  v: number;
  /** Bucket and key of the video. */
  b: string;
  k: string;
  vid?: string;
  /** The viewer, for the application. */
  sub: string;
  /** The text of the watermark. */
  wm?: string;
  /** Session id of the token (its revocation). */
  sid: string;
  /** Issue and expiry, Unix seconds. */
  iat: number;
  exp: number;
}

export interface PlayerError {
  /** denied (expired, revoked, replaced by a newer stream), too-fast, not-found, unsupported, failed. */
  code: 'denied' | 'too-fast' | 'not-found' | 'unsupported' | 'failed';
  /** HTTP status of the failed request (0 when none). */
  status: number;
  message: string;
}

export interface PlayerOptions {
  /** The master playlist with its playback token (playlist of the token API). */
  src: string;
  /** The hls.js class for the browsers without native HLS (default: globalThis.Hls). */
  hls?: unknown;
  /** Configuration of hls.js. */
  hlsConfig?: Record<string, unknown>;
  /** Text of the watermark (default: from the token), false for none. */
  watermark?: string | false;
  /** Milliseconds between two moves of the watermark (default 5000). */
  watermarkInterval?: number;
  autoplay?: boolean;
  muted?: boolean;
  controls?: boolean;
  poster?: string;
  /** Called on a fatal error (default: a message over the video). */
  onError?: (err: PlayerError) => void;
}

export interface Player {
  readonly video: HTMLVideoElement;
  /** Plays another playlist (a renewed token). */
  load(src: string): void;
  /** Stops and removes the player. */
  destroy(): void;
}

export function createPlayer(container: HTMLElement, options: PlayerOptions): Player;
export function playerError(status: number, details: string): PlayerError;
export function startWatermark(stage: HTMLElement, text: string, everyMs?: number): () => void;
export function decodeToken(token: string | null | undefined): PlaybackClaims | undefined;
export function playlistClaims(playlist: string | URL): PlaybackClaims | undefined;
export function watermarkText(claims: Partial<PlaybackClaims> | undefined): string | undefined;

export class NexoPlayerElement extends HTMLElement {
  static Hls: unknown;
  readonly player: Player | undefined;
}
/** Registers <nexo-player src="..." watermark="..." autoplay muted poster="...">. */
export function defineNexoPlayer(hls?: unknown): void;

declare global {
  interface HTMLElementTagNameMap {
    'nexo-player': NexoPlayerElement;
  }
}
