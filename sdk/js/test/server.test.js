import assert from 'node:assert/strict';
import { test } from 'node:test';

import { createNexoClient } from '../src/server.js';

test('issues and revokes playback tokens with signed Admin API calls', async () => {
  const calls = [];
  const fetch = async (url, init) => {
    calls.push({ url: String(url), init });
    if (init.method === 'POST') {
      return new Response(JSON.stringify({ token: 't', sessionId: 's1', expiresAt: '2026-01-01T02:00:00Z', playlist: 'p', player: 'q' }), { status: 201 });
    }
    return new Response(null, { status: 204 });
  };
  const nexo = createNexoClient({ endpoint: 'https://nexo.example.com:9000', accessKey: 'ak', secretKey: 'sk', region: 'eu-south-1', fetch });
  const tok = await nexo.issuePlaybackToken({ bucket: 'lms', key: 'courses/a b.mp4', subject: 'student-7', watermark: 'Mario' });
  assert.equal(tok.sessionId, 's1');
  assert.ok(tok.expiresAt instanceof Date);
  assert.equal(calls[0].url, 'https://nexo.example.com:9000/api/v1/buckets/lms/objects/playback-tokens?key=courses%2Fa+b.mp4');
  assert.deepEqual(JSON.parse(calls[0].init.body), { subject: 'student-7', watermark: 'Mario' });
  assert.match(calls[0].init.headers.authorization, /^AWS4-HMAC-SHA256 Credential=ak\/\d{8}\/eu-south-1\/s3\/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, /);

  await nexo.revokePlaybackSession('lms', 's1');
  await nexo.revokePlaybackSubject('lms', 'student 7');
  assert.equal(calls[1].url, 'https://nexo.example.com:9000/api/v1/buckets/lms/media/playback-sessions/s1');
  assert.equal(calls[2].url, 'https://nexo.example.com:9000/api/v1/buckets/lms/media/playback-subjects/student%207');
  assert.equal(calls[2].init.method, 'DELETE');
});

test('reports the problem of a refused call', async () => {
  const fetch = async () => new Response(JSON.stringify({ title: 'Forbidden', detail: 'no policy allows nexo:IssuePlaybackToken' }), { status: 403 });
  const nexo = createNexoClient({ endpoint: 'http://localhost:9000', accessKey: 'ak', secretKey: 'sk', fetch });
  await assert.rejects(nexo.issuePlaybackToken({ bucket: 'b', key: 'k.mp4', subject: 's' }), (err) => err.status === 403 && /nexo:IssuePlaybackToken/.test(err.message));
});

test('requires the credentials', () => {
  assert.throws(() => createNexoClient({ endpoint: 'http://x' }), TypeError);
});
