import assert from 'node:assert/strict';
import { test } from 'node:test';

import { decodeToken, playlistClaims, watermarkText } from '../src/token.js';

const token = (claims) => Buffer.from(JSON.stringify(claims)).toString('base64url') + '.c2ln';

test('reads the claims of the token of a playlist', () => {
  const url = `https://nexo.example.com/nexo/media/v1/lms/master.m3u8?key=a.mp4&pt=${token({ sub: 's7', wm: 'Mario Rossi · è', exp: 1 })}`;
  const claims = playlistClaims(url);
  assert.equal(claims.sub, 's7');
  assert.equal(watermarkText(claims), 'Mario Rossi · è');
  assert.equal(watermarkText({ sub: 's7' }), 's7');
  assert.equal(watermarkText(undefined), undefined);
  assert.equal(decodeToken('%%%.x'), undefined);
  assert.equal(decodeToken(undefined), undefined);
  assert.equal(playlistClaims('https://nexo.example.com/x'), undefined);
});
