/** Header the Go session service expects (session.CSRFHeader). */
export const defaultCsrfHeaderName = 'X-CSRF-Token';

/**
 * Reads a cookie value from a document.cookie string. Returns undefined when
 * the cookie is absent. Values are percent-decoded.
 */
export function readCookie(name: string, cookieString: string): string | undefined {
  if (cookieString === '') {
    return undefined;
  }
  for (const part of cookieString.split(';')) {
    const trimmed = part.trim();
    const eq = trimmed.indexOf('=');
    if (eq === -1) {
      continue;
    }
    if (trimmed.slice(0, eq) === name) {
      try {
        return decodeURIComponent(trimmed.slice(eq + 1));
      } catch {
        return trimmed.slice(eq + 1);
      }
    }
  }
  return undefined;
}
