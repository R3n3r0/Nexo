import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { api, problemResponse } from '@/test/handlers';
import { server } from '@/test/server';

import { ApiError } from '../errors';
import { createHttpServices } from './index';

/** Lifecycle, events, CORS, Object Lock, attributes, KMS and upload encryption over HTTP. */
describe('M7 HTTP services', () => {
  it('reads and writes the lifecycle rules and applies them', async () => {
    const calls: string[] = [];
    const rules = { rules: [{ id: 'r', enabled: true, expiration: { days: 1 } }] };
    server.use(
      http.get(api('/buckets/a%20b/lifecycle'), () => HttpResponse.json(rules)),
      http.put(api('/buckets/a%20b/lifecycle'), async ({ request }) => {
        calls.push(`PUT ${await request.text()}`);
        return HttpResponse.json(rules);
      }),
      http.delete(api('/buckets/a%20b/lifecycle'), () => {
        calls.push('DELETE');
        return new HttpResponse(null, { status: 204 });
      }),
      http.post(api('/buckets/a%20b/lifecycle/apply'), () =>
        HttpResponse.json({
          buckets: 1,
          versions: 3,
          deleted: 1,
          deleteMarkers: 0,
          uploadsAborted: 0,
          locked: 0,
        }),
      ),
    );
    const { lifecycle } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await lifecycle.get('a b')).toEqual(rules);
    expect(await lifecycle.set('a b', rules)).toEqual(rules);
    await lifecycle.remove('a b');
    expect((await lifecycle.apply('a b')).deleted).toBe(1);
    expect(calls).toEqual([`PUT ${JSON.stringify(rules)}`, 'DELETE']);
  });

  it('decodes the rule errors of an invalid configuration', async () => {
    server.use(
      http.put(api('/buckets/b/lifecycle'), () =>
        HttpResponse.json(
          {
            title: 'Bad Request',
            status: 400,
            code: 'invalid_configuration',
            detail: 'x',
            ruleErrors: [
              { rule: 2, id: 'r2', code: 'InvalidArgument', message: 'bad days' },
              { bogus: true },
            ],
          },
          { status: 400, headers: { 'content-type': 'application/problem+json' } },
        ),
      ),
    );
    const { lifecycle } = createHttpServices({ baseUrl: '/api/v1' });
    const error = await lifecycle.set('b', { rules: [] }).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).ruleErrors).toEqual([
      { rule: 2, id: 'r2', code: 'InvalidArgument', message: 'bad days' },
    ]);
  });

  it('manages the event subscriptions and tests a target by reference', async () => {
    const tested: string[] = [];
    server.use(
      http.get(api('/buckets/b/events'), () => HttpResponse.json({ subscriptions: [] })),
      http.get(api('/notify/targets'), () =>
        HttpResponse.json({
          items: [
            {
              id: '_',
              type: 'webhook',
              arn: 'arn:nexo:sqs:us-east-1:_:webhook',
              enabled: true,
              online: true,
              pendingEvents: 0,
            },
          ],
        }),
      ),
      http.post(api('/notify/targets/:id/test'), ({ params }) => {
        tested.push(String(params.id));
        return HttpResponse.json({ target: '_:webhook', delivered: true });
      }),
    );
    const { notifications } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await notifications.get('b')).toEqual({ subscriptions: [] });
    expect((await notifications.targets()).map((t) => t.arn)).toEqual([
      'arn:nexo:sqs:us-east-1:_:webhook',
    ]);
    expect(await notifications.test('_:webhook')).toEqual({ target: '_:webhook', delivered: true });
    expect(tested).toEqual(['_:webhook']);
  });

  it('removes the CORS configuration when no rule is left', async () => {
    const calls: string[] = [];
    server.use(
      http.put(api('/buckets/b/cors'), async ({ request }) => {
        calls.push(`PUT ${await request.text()}`);
        return HttpResponse.json({ rules: [{ allowedOrigins: ['*'], allowedMethods: ['GET'] }] });
      }),
      http.delete(api('/buckets/b/cors'), () => {
        calls.push('DELETE');
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { buckets } = createHttpServices({ baseUrl: '/api/v1' });
    await buckets.setCors('b', { rules: [{ allowedOrigins: ['*'], allowedMethods: ['GET'] }] });
    expect(await buckets.setCors('b', { rules: [] })).toEqual({ rules: [] });
    expect(calls).toEqual([
      'PUT {"rules":[{"allowedOrigins":["*"],"allowedMethods":["GET"]}]}',
      'DELETE',
    ]);
  });

  it('reads and sets the retention and legal hold of a version', async () => {
    const seen: string[] = [];
    server.use(
      http.get(api('/buckets/b/objects/retention'), ({ request }) => {
        seen.push(new URL(request.url).search);
        return HttpResponse.json({
          versionId: 'v1',
          mode: 'GOVERNANCE',
          retainUntil: '2030-01-01T00:00:00Z',
        });
      }),
      http.put(api('/buckets/b/objects/retention'), async ({ request }) => {
        seen.push(await request.text());
        return HttpResponse.json({ versionId: 'v1' });
      }),
      http.put(api('/buckets/b/objects/legal-hold'), async ({ request }) => {
        seen.push(await request.text());
        return HttpResponse.json({ versionId: 'v1', status: 'on' });
      }),
    );
    const { objects } = createHttpServices({ baseUrl: '/api/v1' });
    expect((await objects.getRetention('b', 'a b', 'v1')).mode).toBe('GOVERNANCE');
    await objects.setRetention('b', 'a b', { bypassGovernance: true }, 'v1');
    expect(await objects.setLegalHold('b', 'a b', 'ON', 'v1')).toBe('ON');
    expect(seen).toEqual(['?key=a+b&versionId=v1', '{"bypassGovernance":true}', '{"status":"ON"}']);
  });

  it('pages the parts of the attributes of a version', async () => {
    let query: URLSearchParams | undefined;
    server.use(
      http.get(api('/buckets/b/objects/attributes'), ({ request }) => {
        query = new URL(request.url).searchParams;
        return HttpResponse.json({
          key: 'k',
          etag: 'e',
          size: 1,
          lastModified: '2026-10-01T00:00:00Z',
          partsCount: 3,
          parts: [{ partNumber: 3, size: 1 }],
          isTruncated: false,
        });
      }),
    );
    const { objects } = createHttpServices({ baseUrl: '/api/v1' });
    const attrs = await objects.attributes('b', 'k', { maxParts: 2, partNumberMarker: 2 });
    expect(attrs.partsCount).toBe(3);
    expect(query?.get('maxParts')).toBe('2');
    expect(query?.get('partNumberMarker')).toBe('2');
    expect(query?.has('versionId')).toBe(false);
  });

  it('reads the KMS and creates a key', async () => {
    server.use(
      http.get(api('/kms/status'), () =>
        HttpResponse.json({ configured: true, backend: 'builtin', online: true }),
      ),
      http.get(api('/kms/keys'), ({ request }) =>
        HttpResponse.json({
          items: [{ id: `${new URL(request.url).searchParams.get('prefix') ?? ''}key` }],
        }),
      ),
      http.post(api('/kms/keys'), () => problemResponse(409, 'conflict', 'kms key already exists')),
    );
    const { kms } = createHttpServices({ baseUrl: '/api/v1' });
    expect((await kms.status()).configured).toBe(true);
    expect(await kms.keys('app-')).toEqual([{ id: 'app-key' }]);
    await expect(kms.createKey('app-key')).rejects.toMatchObject({ status: 409 });
  });

  it('asks for the encryption of a multipart upload with the S3 headers', async () => {
    const headers: Record<string, string | null>[] = [];
    server.use(
      http.post(api('/buckets/b/uploads'), async ({ request }) => {
        headers.push({
          sse: request.headers.get('x-amz-server-side-encryption'),
          key: request.headers.get('x-amz-server-side-encryption-aws-kms-key-id'),
          body: await request.text(),
        });
        return HttpResponse.json(
          {
            bucket: 'b',
            key: 'k',
            uploadId: 'u',
            initiated: '2026-10-01T00:00:00Z',
            partSize: 1,
            minPartSize: 1,
            maxPartSize: 1,
            maxParts: 1,
          },
          { status: 201 },
        );
      }),
    );
    const { uploads } = createHttpServices({ baseUrl: '/api/v1' });
    await uploads.create('b', 'k', {
      contentType: 'text/plain',
      encryption: { algorithm: 'aws:kms', kmsKeyId: 'app-key' },
    });
    await uploads.create('b', 'k', { encryption: { algorithm: 'AES256', kmsKeyId: 'ignored' } });
    await uploads.create('b', 'k');
    expect(headers).toEqual([
      { sse: 'aws:kms', key: 'app-key', body: '{"contentType":"text/plain"}' },
      { sse: 'AES256', key: null, body: '{}' },
      { sse: null, key: null, body: '{}' },
    ]);
  });
});
