// Playback tokens of Nexo: base64url(JSON claims).base64url(signature).
// The claims are not secret (the player reads the watermark from them);
// only the server can sign or verify a token.

/**
 * The claims of a playback token, or undefined for a malformed one.
 * @param {string | null | undefined} token
 * @returns {import('./index.js').PlaybackClaims | undefined}
 */
export function decodeToken(token) {
  const payload = token ? token.split('.')[0] : '';
  if (!payload) return undefined;
  try {
    const b64 = payload.replace(/-/g, '+').replace(/_/g, '/');
    const padded = b64 + '='.repeat((4 - (b64.length % 4)) % 4);
    const bytes = Uint8Array.from(atob(padded), (c) => c.charCodeAt(0));
    const claims = JSON.parse(new TextDecoder().decode(bytes));
    return claims && typeof claims === 'object' ? claims : undefined;
  } catch {
    return undefined;
  }
}

/**
 * The claims of the token (pt parameter) of a playlist URL.
 * @param {string | URL} playlist
 */
export function playlistClaims(playlist) {
  try {
    const url = new URL(String(playlist), typeof location === 'undefined' ? 'http://localhost/' : location.href);
    return decodeToken(url.searchParams.get('pt'));
  } catch {
    return undefined;
  }
}

/**
 * The text of the watermark of a token: its watermark, else its subject.
 * @param {import('./index.js').PlaybackClaims | undefined} claims
 * @returns {string | undefined}
 */
export function watermarkText(claims) {
  const text = String(claims?.wm ?? claims?.sub ?? '').trim();
  return text === '' ? undefined : text;
}
