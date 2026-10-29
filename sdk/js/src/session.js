// The session of the player of Nexo (the advanced playback of the
// protected videos): the player and the server negotiate a wrapping key
// with ephemeral ECDH P-256 keys (WebCrypto) over a WebSocket, the keys of
// the segments arrive wrapped with it and are unwrapped into CryptoKeys
// the page cannot read back (extractable: false), and every segment,
// encrypted for this session alone (AES-128-GCM), is decrypted before the
// player hands it to the browser. Behind a proxy without WebSocket, the
// same messages travel as POST requests; a channel that drops continues
// that way (the ticket of the session is valid on every node).

const HELLO_TIMEOUT = 10_000;
const WRAP_INFO = 'nexo/media/session-wrap-v1\u0000';
const encoder = new TextEncoder();

/**
 * The bytes of a base64url string.
 * @param {string} text
 */
function fromBase64url(text) {
  const b64 = text.replace(/-/g, '+').replace(/_/g, '/');
  const padded = b64 + '='.repeat((4 - (b64.length % 4)) % 4);
  return Uint8Array.from(atob(padded), (c) => c.charCodeAt(0));
}

/**
 * The base64url of bytes.
 * @param {ArrayBuffer | Uint8Array} bytes
 */
function toBase64url(bytes) {
  let s = '';
  for (const b of new Uint8Array(bytes)) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/**
 * The error of a session, in the form of the errors of the player.
 * @param {string} code
 * @param {number} status
 * @param {string} message
 * @returns {import('./index.js').PlayerError}
 */
function sessionError(code, status, message) {
  return { code: /** @type {any} */ (code), status, message };
}

/**
 * The additional data a wrapped key is bound to (its presentation and its
 * period), as the server builds it.
 * @param {string} rev
 * @param {string} rendition
 * @param {number} period
 */
export function keyAAD(rev, rendition, period) {
  return `nexo/media/key\u0000${rev}\u0000${rendition}\u0000${period}`;
}

/**
 * The URL of the session channel of a master playlist with a playback
 * token (…/master.m3u8?key=…&pt=…): …/session?key=…&versionId=…&pt=….
 * @param {string | URL} playlist
 */
export function sessionURL(playlist) {
  const src = new URL(String(playlist), typeof location === 'undefined' ? 'http://localhost/' : location.href);
  const url = new URL(src.pathname.replace(/\/master\.m3u8$/, '/session'), src);
  for (const name of ['key', 'versionId', 'pt']) {
    const v = src.searchParams.get(name);
    if (v !== null) url.searchParams.set(name, v);
  }
  return url;
}

/**
 * Whether a URL is a media segment of Nexo (encrypted in a session).
 * @param {string} url
 */
export function isSegmentURL(url) {
  try {
    return new URL(url, 'http://localhost/').pathname.endsWith('/segment.m4s');
  } catch {
    return false;
  }
}

/**
 * Opens a session for a master playlist with a playback token.
 * @param {string | URL} playlist
 * @param {import('./index.js').SessionOptions} [options]
 * @returns {Promise<import('./index.js').Session>}
 */
export async function openSession(playlist, options = {}) {
  const subtle = (options.crypto ?? globalThis.crypto).subtle;
  const WS = options.WebSocket === undefined ? globalThis.WebSocket : options.WebSocket;
  const doFetch = options.fetch ?? globalThis.fetch.bind(globalThis);
  const endpoint = sessionURL(playlist);

  // The ephemeral key of the player: the private half cannot leave
  // WebCrypto.
  const pair = await subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, false, ['deriveBits']);
  const pub = await subtle.exportKey('raw', pair.publicKey);
  const hello = JSON.stringify({ t: 'hello', v: 1, pub: toBase64url(pub) });

  /** @type {Map<number, {resolve: (m: any) => void, reject: (e: any) => void}>} */
  const pending = new Map();
  /** @type {Map<string, Promise<CryptoKey>>} */
  const keys = new Map();
  let nextID = 1;
  /** @type {WebSocket | undefined} */
  let ws;
  let closed = false;
  /** @type {any} */
  let ready;

  const failPending = (/** @type {any} */ err) => {
    for (const p of pending.values()) p.reject(err);
    pending.clear();
  };
  const onMessage = (/** @type {any} */ m) => {
    if (m.t === 'key' || m.t === 'error') {
      const p = pending.get(m.id);
      if (!p) return;
      pending.delete(m.id);
      if (m.t === 'key') p.resolve(m);
      else p.reject(sessionError(m.code ?? 'failed', m.status ?? 0, m.message ?? 'key refused'));
    } else if (m.t === 'closed') {
      closed = true;
      const err = sessionError(m.code ?? 'denied', m.status ?? 403, m.message ?? 'the session was closed');
      failPending(err);
      options.onClosed?.(err);
      ws?.close();
    }
  };

  // The channel: a WebSocket when one opens, else POST requests.
  const viaWebSocket = () =>
    new Promise((resolve, reject) => {
      if (!WS) {
        reject(new Error('no WebSocket'));
        return;
      }
      const url = new URL(endpoint);
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
      const socket = new WS(url.toString());
      let opened = false;
      const timer = setTimeout(() => {
        socket.close();
        reject(sessionError('failed', 0, 'the session did not open in time'));
      }, HELLO_TIMEOUT);
      socket.onopen = () => {
        opened = true;
        socket.send(hello);
      };
      socket.onmessage = (/** @type {MessageEvent} */ e) => {
        let m;
        try {
          m = JSON.parse(String(e.data));
        } catch {
          return;
        }
        if (!ready && m.t === 'ready') {
          clearTimeout(timer);
          ws = socket;
          resolve(m);
          return;
        }
        if (!ready && m.t === 'closed') {
          clearTimeout(timer);
          closed = true;
          reject(sessionError(m.code ?? 'denied', m.status ?? 403, m.message ?? 'the session was refused'));
          return;
        }
        onMessage(m);
      };
      socket.onerror = () => {
        if (!opened) {
          clearTimeout(timer);
          reject(new Error('no WebSocket'));
        }
      };
      socket.onclose = () => {
        clearTimeout(timer);
        if (ws === socket) {
          // Dropped (or the node stopped): the keys go on by POST.
          ws = undefined;
        }
        if (!ready && opened) reject(sessionError('denied', 403, 'the session was refused'));
      };
    });
  const viaPost = async () => {
    const resp = await doFetch(endpoint.toString(), { method: 'POST', body: hello, headers: { 'Content-Type': 'text/plain' } });
    if (!resp.ok) throw await httpError(resp);
    return resp.json();
  };
  const httpError = async (/** @type {Response} */ resp) => {
    let message = `HTTP ${resp.status}`;
    try {
      const body = await resp.text();
      const m = /<Message>([^<]*)<\/Message>/.exec(body) ?? /"message":"([^"]*)"/.exec(body);
      if (m) message = m[1];
    } catch {
      // the status says enough
    }
    const code = resp.status === 403 || resp.status === 401 ? 'denied' : resp.status === 429 ? 'too-fast' : resp.status === 404 ? 'not-found' : 'failed';
    return sessionError(code, resp.status, message);
  };

  try {
    ready = await viaWebSocket();
  } catch (err) {
    if (/** @type {any} */ (err)?.code) throw err;
    ready = await viaPost();
  }
  if (!ready || ready.t !== 'ready') throw sessionError('failed', 0, 'unexpected answer of the session');

  // The wrapping key of the session: ECDH, then HKDF-SHA256 with the salt
  // and the session id; usable only to unwrap.
  const serverKey = await subtle.importKey('raw', fromBase64url(ready.pub), { name: 'ECDH', namedCurve: 'P-256' }, false, []);
  const secret = await subtle.deriveBits({ name: 'ECDH', public: serverKey }, pair.privateKey, 256);
  const hkdf = await subtle.importKey('raw', secret, 'HKDF', false, ['deriveKey']);
  const wrapKey = await subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt: fromBase64url(ready.salt), info: encoder.encode(WRAP_INFO + ready.session) },
    hkdf,
    { name: 'AES-GCM', length: 128 },
    false,
    ['unwrapKey'],
  );
  const period = ready.period > 0 ? ready.period : 10;
  const master = new URL(ready.master, endpoint).toString();
  const keyEndpoint = new URL(endpoint.pathname + '/key', endpoint);
  for (const name of ['key', 'versionId']) {
    const v = endpoint.searchParams.get(name);
    if (v !== null) keyEndpoint.searchParams.set(name, v);
  }
  keyEndpoint.searchParams.set('st', ready.ticket);

  const requestKey = async (/** @type {string} */ rev, /** @type {string} */ r, /** @type {number} */ i) => {
    if (closed) throw sessionError('denied', 403, 'the session was closed');
    const req = { t: 'key', id: nextID++, rev, r, i };
    if (ws && ws.readyState === 1) {
      return new Promise((resolve, reject) => {
        pending.set(req.id, { resolve, reject });
        ws?.send(JSON.stringify(req));
      });
    }
    const resp = await doFetch(keyEndpoint.toString(), { method: 'POST', body: JSON.stringify(req), headers: { 'Content-Type': 'text/plain' } });
    if (!resp.ok) throw await httpError(resp);
    return resp.json();
  };

  /** @type {import('./index.js').Session} */
  const session = {
    id: ready.session,
    master,
    expiresAt: ready.exp,
    period,
    key(rev, rendition, i) {
      const id = `${rev}\u0000${rendition}\u0000${i}`;
      let p = keys.get(id);
      if (!p) {
        p = requestKey(rev, rendition, i).then((m) => {
          const sealed = fromBase64url(m.key);
          return subtle.unwrapKey(
            'raw',
            sealed.subarray(12),
            wrapKey,
            { name: 'AES-GCM', iv: sealed.subarray(0, 12), additionalData: encoder.encode(keyAAD(rev, rendition, i)) },
            { name: 'AES-GCM' },
            false,
            ['decrypt'],
          );
        });
        // A refused key is asked again later (a pace limit passes).
        p.catch(() => keys.delete(id));
        keys.set(id, p);
      }
      return p;
    },
    async decrypt(url, data) {
      const u = new URL(url, master);
      const n = Number(u.searchParams.get('n'));
      const rev = u.searchParams.get('rev') ?? '';
      const rendition = u.searchParams.get('r') ?? '';
      const key = await session.key(rev, rendition, Math.floor(n / period));
      const bytes = new Uint8Array(data);
      return subtle.decrypt(
        { name: 'AES-GCM', iv: bytes.subarray(0, 12), additionalData: encoder.encode(String(n)) },
        key,
        bytes.subarray(12),
      );
    },
    close() {
      closed = true;
      failPending(sessionError('failed', 0, 'the session was closed'));
      ws?.close();
      ws = undefined;
    },
  };
  return session;
}

/**
 * A loader class for hls.js (its fLoader) that decrypts the segments of a
 * session before hls.js sees them; the other requests pass through.
 * @param {any} Hls the hls.js class
 * @param {import('./index.js').Session} session
 */
export function sessionLoader(Hls, session) {
  const Base = Hls.DefaultConfig.loader;
  return class SessionLoader extends Base {
    /**
     * @param {any} context
     * @param {any} config
     * @param {any} callbacks
     */
    load(context, config, callbacks) {
      if (!isSegmentURL(context.url)) {
        super.load(context, config, callbacks);
        return;
      }
      super.load(context, config, {
        ...callbacks,
        onSuccess: (/** @type {any} */ response, /** @type {any} */ stats, /** @type {any} */ ctx, /** @type {any} */ details) => {
          session.decrypt(context.url, response.data).then(
            (data) => callbacks.onSuccess({ ...response, data }, stats, ctx, details),
            (err) => callbacks.onError({ code: err?.status ?? 0, text: err?.message ?? 'decryption failed' }, ctx, details, stats),
          );
        },
      });
    }
  };
}
