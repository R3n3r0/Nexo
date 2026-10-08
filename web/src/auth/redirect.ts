/** Route the login page falls back to after a successful sign-in. */
export const defaultAfterLogin = '/';

/**
 * Extracts the route to return to from the router location state set by
 * AuthGuard. Only internal, absolute paths are accepted so that a crafted
 * state can never redirect to another origin.
 */
export function redirectTargetFromState(state: unknown): string {
  if (typeof state !== 'object' || state === null) {
    return defaultAfterLogin;
  }
  const from = (state as { from?: unknown }).from;
  if (typeof from !== 'string') {
    return defaultAfterLogin;
  }
  return sanitizeRedirect(from);
}

/** Keeps only same-origin paths ("/x"), excluding the login page itself. */
export function sanitizeRedirect(path: string): string {
  if (!path.startsWith('/') || path.startsWith('//') || path.startsWith('/\\')) {
    return defaultAfterLogin;
  }
  if (path === '/login' || path.startsWith('/login?') || path.startsWith('/login/')) {
    return defaultAfterLogin;
  }
  return path;
}
