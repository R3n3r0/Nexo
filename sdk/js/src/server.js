// Backend side of the SDK (Node.js 18+, Deno, Bun): issue and revoke the
// playback tokens of the videos with the Admin API of Nexo. The access
// key never reaches the browser: the page receives only the playlist URL
// with its token.
import { signRequest } from './sigv4.js';

export { signRequest } from './sigv4.js';

/**
 * A client of the playback tokens of a Nexo server.
 * @param {import('./server.js').NexoConfig} config
 */
export function createNexoClient(config) {
  if (!config?.endpoint || !config.accessKey || !config.secretKey) {
    throw new TypeError('nexo: endpoint, accessKey and secretKey are required');
  }
  const base = new URL(config.endpoint);
  const doFetch = config.fetch ?? globalThis.fetch;

  /**
   * @param {string} method
   * @param {string[]} segments path segments under /api/v1, escaped one by one
   * @param {Record<string,string>} [query]
   * @param {unknown} [body]
   */
  async function call(method, segments, query, body) {
    const url = new URL(base.toString());
    url.pathname = base.pathname.replace(/\/$/, '') + '/api/v1/' + segments.map((s) => encodeURIComponent(s)).join('/');
    for (const [k, v] of Object.entries(query ?? {})) if (v) url.searchParams.set(k, v);
    const payload = body === undefined ? '' : JSON.stringify(body);
    const headers = signRequest({
      method, url, body: payload, accessKey: config.accessKey, secretKey: config.secretKey,
      sessionToken: config.sessionToken, region: config.region,
      headers: body === undefined ? {} : { 'content-type': 'application/json' },
    });
    const res = await doFetch(url, { method, headers, body: body === undefined ? undefined : payload });
    const text = await res.text();
    if (!res.ok) {
      let detail = text;
      try {
        const problem = JSON.parse(text);
        detail = problem.detail || problem.title || text;
      } catch {
        // not JSON
      }
      const err = new Error(`nexo: ${method} ${url.pathname}: ${res.status} ${detail}`);
      Object.assign(err, { status: res.status });
      throw err;
    }
    return text ? JSON.parse(text) : undefined;
  }

  return {
    /**
     * Issues a playback token of a video for a viewer: hand `playlist` to
     * the player (or `player`, the page of the Nexo player, to an iframe).
     * @param {import('./server.js').IssueOptions} opts
     * @returns {Promise<import('./server.js').PlaybackToken>}
     */
    async issuePlaybackToken(opts) {
      const out = await call('POST', ['buckets', opts.bucket, 'objects', 'playback-tokens'], { key: opts.key, versionId: opts.versionId ?? '' },
        { subject: opts.subject, watermark: opts.watermark, ttlSeconds: opts.ttlSeconds });
      return { ...out, expiresAt: new Date(out.expiresAt) };
    },
    /** Revokes one token by its session id. */
    async revokePlaybackSession(/** @type {string} */ bucket, /** @type {string} */ sessionId) {
      await call('DELETE', ['buckets', bucket, 'media', 'playback-sessions', sessionId]);
    },
    /** Revokes every token of the videos of a bucket issued to a viewer so far. */
    async revokePlaybackSubject(/** @type {string} */ bucket, /** @type {string} */ subject) {
      await call('DELETE', ['buckets', bucket, 'media', 'playback-subjects', subject]);
    },
  };
}
