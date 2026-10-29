/** Claims of a playback token the player reads (the payload is not secret). */
export interface PlaybackClaims {
  sub?: string;
  wm?: string;
  exp?: number;
}

/**
 * The claims of the playback token (pt) of a master playlist URL:
 * base64url(JSON).signature. Undefined without token or for a malformed
 * one (the server refuses it anyway).
 */
export function playbackClaims(url: URL): PlaybackClaims | undefined {
  const token = url.searchParams.get('pt');
  const payload = token?.split('.')[0];
  if (!payload) return undefined;
  try {
    const b64 = payload.replace(/-/g, '+').replace(/_/g, '/');
    const bytes = Uint8Array.from(atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4)), (c) =>
      c.charCodeAt(0),
    );
    const claims: unknown = JSON.parse(new TextDecoder().decode(bytes));
    return typeof claims === 'object' && claims !== null ? claims : undefined;
  } catch {
    return undefined;
  }
}

/** The text of the watermark of a token: its watermark, else its subject. */
export function watermarkText(claims: PlaybackClaims | undefined): string | undefined {
  const text = (claims?.wm ?? claims?.sub ?? '').trim();
  return text === '' ? undefined : text;
}

/**
 * Draws text over stage, moving it to a random place every few seconds
 * (a fixed mark is cropped out of a recording, a moving one is not).
 * Returns the function that removes it.
 */
export function startWatermark(
  stage: HTMLElement,
  text: string,
  everyMs = 5000,
  random: () => number = Math.random,
): () => void {
  const mark = document.createElement('div');
  mark.className = 'watermark';
  mark.textContent = text;
  mark.setAttribute('aria-hidden', 'true');
  stage.appendChild(mark);
  const move = () => {
    mark.style.left = `${String(Math.round(5 + random() * 60))}%`;
    mark.style.top = `${String(Math.round(5 + random() * 80))}%`;
  };
  move();
  const timer = window.setInterval(move, everyMs);
  return () => {
    window.clearInterval(timer);
    mark.remove();
  };
}
