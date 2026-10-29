// The session of the player against a fake server that follows the
// protocol of Nexo (internal/transport/http/s3/media_session.go): ECDH
// P-256, HKDF-SHA256, keys wrapped with AES-128-GCM, segments sealed with
// AES-128-GCM bound to their number.
import assert from 'node:assert/strict';
import { createCipheriv, createECDH, hkdfSync, randomBytes } from 'node:crypto';
import { test } from 'node:test';

import { isSegmentURL, keyAAD, openSession, sessionURL } from '../src/session.js';

const b64 = (buf) => Buffer.from(buf).toString('base64url');

function seal(key, plain, aad) {
  const nonce = randomBytes(12);
  const c = createCipheriv('aes-128-gcm', key, nonce);
  c.setAAD(Buffer.from(aad));
  const body = Buffer.concat([c.update(plain), c.final()]);
  return Buffer.concat([nonce, body, c.getAuthTag()]);
}

// A fake server: its answers to the hello and the key requests.
function fakeServer() {
  const server = { keys: new Map(), requests: [], closed: false };
  server.hello = (msg) => {
    assert.equal(msg.t, 'hello');
    assert.equal(msg.v, 1);
    const ecdh = createECDH('prime256v1');
    ecdh.generateKeys();
    const secret = ecdh.computeSecret(Buffer.from(msg.pub, 'base64url'));
    const salt = randomBytes(16);
    const session = randomBytes(16).toString('hex');
    server.wrap = Buffer.from(hkdfSync('sha256', secret, salt, 'nexo/media/session-wrap-v1\u0000' + session, 16));
    return {
      t: 'ready', v: 1, pub: b64(ecdh.getPublicKey()), salt: b64(salt), session, ticket: 's1.ticket',
      master: '/nexo/media/v1/lms/master.m3u8?key=a.mp4&st=s1.ticket', exp: 2_000_000_000, period: 10,
    };
  };
  server.key = (msg) => {
    server.requests.push(msg);
    const key = randomBytes(16);
    server.keys.set(`${msg.rev}|${msg.r}|${msg.i}`, key);
    return { t: 'key', id: msg.id, key: b64(seal(server.wrap, key, keyAAD(msg.rev, msg.r, msg.i))) };
  };
  return server;
}

// A fake WebSocket class bound to a server.
function fakeWebSocket(server, { fail = false } = {}) {
  return class {
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      server.wsURL = url;
      setTimeout(() => {
        if (fail) {
          this.onerror?.();
          this.onclose?.();
          return;
        }
        this.readyState = 1;
        this.onopen?.();
      });
      server.socket = this;
    }
    send(data) {
      const msg = JSON.parse(data);
      const reply = msg.t === 'hello' ? server.hello(msg) : server.key(msg);
      setTimeout(() => this.onmessage?.({ data: JSON.stringify(reply) }));
    }
    push(msg) {
      this.onmessage?.({ data: JSON.stringify(msg) });
    }
    close() {
      this.readyState = 3;
      server.closed = true;
    }
  };
}

const playlist = 'https://nexo.example.com/nexo/media/v1/lms/master.m3u8?key=a.mp4&pt=tok';

test('sessionURL and isSegmentURL', () => {
  assert.equal(sessionURL(playlist).toString(), 'https://nexo.example.com/nexo/media/v1/lms/session?key=a.mp4&pt=tok');
  assert.ok(isSegmentURL('https://x/nexo/media/v1/lms/segment.m4s?n=3'));
  assert.ok(!isSegmentURL('https://x/nexo/media/v1/lms/init.mp4'));
});

test('a session over the WebSocket unwraps the keys and decrypts the segments', async () => {
  const server = fakeServer();
  const s = await openSession(playlist, { WebSocket: fakeWebSocket(server) });
  assert.equal(server.wsURL, 'wss://nexo.example.com/nexo/media/v1/lms/session?key=a.mp4&pt=tok');
  assert.equal(s.master, 'https://nexo.example.com/nexo/media/v1/lms/master.m3u8?key=a.mp4&st=s1.ticket');

  const plain = Buffer.from('a media segment, fMP4 moof and mdat');
  const key = await s.key('e1', '720p', 1);
  assert.equal(key.extractable, false, 'the page cannot read the key back');
  await assert.rejects(globalThis.crypto.subtle.exportKey('raw', key));
  const sealed = seal(server.keys.get('e1|720p|1'), plain, '12');
  const out = await s.decrypt('https://nexo.example.com/nexo/media/v1/lms/segment.m4s?key=a.mp4&rev=e1&r=720p&n=12&st=x', sealed);
  assert.deepEqual(Buffer.from(out), plain);
  // Same period, same key: asked once.
  await s.decrypt('segment.m4s?rev=e1&r=720p&n=13', seal(server.keys.get('e1|720p|1'), plain, '13'));
  assert.equal(server.requests.length, 1);
  // Bound to the number of the segment.
  await assert.rejects(s.decrypt('segment.m4s?rev=e1&r=720p&n=14', sealed));
  s.close();
});

test('without WebSocket the session goes through POST requests', async () => {
  const server = fakeServer();
  const calls = [];
  const fetch = async (url, init) => {
    calls.push(url);
    const msg = JSON.parse(init.body);
    const reply = url.includes('/session/key') ? server.key(msg) : server.hello(msg);
    return new Response(JSON.stringify(reply), { status: 200 });
  };
  const s = await openSession(playlist, { WebSocket: fakeWebSocket(server, { fail: true }), fetch });
  await s.key('e1', '', 0);
  assert.deepEqual(calls, [
    'https://nexo.example.com/nexo/media/v1/lms/session?key=a.mp4&pt=tok',
    'https://nexo.example.com/nexo/media/v1/lms/session/key?key=a.mp4&st=s1.ticket',
  ]);
});

test('a refusal and the end of a session reach the player', async () => {
  const fetch = async () => new Response('<Error><Code>AccessDenied</Code><Message>the playback token was revoked</Message></Error>', { status: 403 });
  await assert.rejects(openSession(playlist, { WebSocket: null, fetch }), (err) => err.code === 'denied' && err.status === 403 && /revoked/.test(err.message));

  const server = fakeServer();
  let closed;
  const s = await openSession(playlist, { WebSocket: fakeWebSocket(server), onClosed: (err) => (closed = err) });
  server.socket.push({ t: 'closed', code: 'denied', status: 403, message: 'a newer stream of this viewer replaced this one' });
  assert.equal(closed.code, 'denied');
  assert.ok(server.closed);
  await assert.rejects(s.key('e1', '', 0), (err) => err.code === 'denied');
});
