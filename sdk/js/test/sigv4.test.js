import assert from 'node:assert/strict';
import { test } from 'node:test';

import { signRequest } from '../src/sigv4.js';

// The "GET Object" example of the AWS documentation (Signature Version 4,
// header-based authentication).
test('signs the AWS reference request', () => {
  const headers = signRequest({
    method: 'GET',
    url: new URL('https://examplebucket.s3.amazonaws.com/test.txt'),
    headers: { Range: 'bytes=0-9' },
    accessKey: 'AKIAIOSFODNN7EXAMPLE',
    secretKey: 'wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY',
    region: 'us-east-1',
    date: new Date('2013-05-24T00:00:00Z'),
  });
  assert.equal(
    headers.authorization,
    'AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, ' +
      'SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, ' +
      'Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41',
  );
  assert.equal(headers['x-amz-date'], '20130524T000000Z');
});
