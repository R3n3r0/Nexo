import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { api, problemResponse } from '@/test/handlers';
import { server } from '@/test/server';

import { ApiError } from '../errors';
import type { AccessRules, Distribution } from '../types';
import { trafficOf } from './distributions';
import { createHttpServices } from './index';

const rules: AccessRules = {
  bucket: 'media',
  rules: [{ id: 'r1', priority: 10, effect: 'allow', principals: ['*'], actions: ['read'] }],
  etag: 'abc123',
  public: true,
  blockPublic: false,
  warnings: [],
};

/** Access rules, distributions, key groups and certificates over HTTP (docs/10 §10.5). */
describe('M7b HTTP services', () => {
  it('replaces the access rules with If-Match on the etag, quoted', async () => {
    const seen: { ifMatch: string | null; body: string }[] = [];
    server.use(
      http.get(api('/buckets/a%20b/access-rules'), () => HttpResponse.json(rules)),
      http.put(api('/buckets/a%20b/access-rules'), async ({ request }) => {
        seen.push({ ifMatch: request.headers.get('If-Match'), body: await request.text() });
        return HttpResponse.json(rules);
      }),
    );
    const { accessRules } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await accessRules.get('a b')).toEqual(rules);
    await accessRules.set('a b', rules.rules, 'abc123');
    await accessRules.set('a b', []);
    expect(seen).toEqual([
      { ifMatch: '"abc123"', body: JSON.stringify({ rules: rules.rules }) },
      { ifMatch: null, body: JSON.stringify({ rules: [] }) },
    ]);
  });

  it('decodes the located access rule errors and the 412 of a lost race', async () => {
    server.use(
      http.put(api('/buckets/b/access-rules'), ({ request }) =>
        request.headers.get('If-Match') === '"old"'
          ? problemResponse(412, 'precondition_failed', 'changed')
          : problemResponse(400, 'invalid_access_rule', 'bad regex', {
              accessRuleErrors: [
                { rule: 2, id: 'r2', field: 'match.pattern', position: 5, message: 'missing )' },
              ],
            }),
      ),
    );
    const { accessRules } = createHttpServices({ baseUrl: '/api/v1' });
    const invalid = (await accessRules.set('b', []).catch((e: unknown) => e)) as ApiError;
    expect(invalid).toBeInstanceOf(ApiError);
    expect(invalid.accessRuleErrors).toEqual([
      { rule: 2, id: 'r2', field: 'match.pattern', position: 5, message: 'missing )' },
    ]);
    const stale = (await accessRules.set('b', [], 'old').catch((e: unknown) => e)) as ApiError;
    expect(stale.isPreconditionFailed).toBe(true);
  });

  it('tests keys, applies presets and reads the block public access of both levels', async () => {
    const calls: string[] = [];
    const block = { enabled: true, mode: 'reject', cluster: true, effective: true };
    server.use(
      http.post(api('/buckets/b/access-rules/test'), async ({ request }) => {
        calls.push(`test ${await request.text()}`);
        return HttpResponse.json({
          bucket: 'b',
          action: 's3:GetObject',
          blockPublic: block,
          results: [],
        });
      }),
      http.get(api('/buckets/b/access-rules/public'), () =>
        HttpResponse.json({ bucket: 'b', public: false, preset: 'private', blocked: false }),
      ),
      http.put(api('/buckets/b/access-rules/public'), async ({ request }) => {
        calls.push(`preset ${await request.text()} ${request.headers.get('If-Match') ?? ''}`);
        return HttpResponse.json(rules);
      }),
      http.get(api('/access/block-public'), () => HttpResponse.json(block)),
      http.put(api('/access/block-public'), async ({ request }) => {
        calls.push(`cluster ${await request.text()}`);
        return HttpResponse.json(block);
      }),
      http.put(api('/buckets/b/block-public'), async ({ request }) => {
        calls.push(`bucket ${await request.text()}`);
        return HttpResponse.json({ ...block, bucket: 'b' });
      }),
    );
    const { accessRules } = createHttpServices({ baseUrl: '/api/v1' });
    await accessRules.test('b', { keys: ['a.txt'], principal: 'anonymous' });
    expect((await accessRules.publicStatus('b')).preset).toBe('private');
    await accessRules.applyPreset(
      'b',
      { preset: 'public-prefix', match: { type: 'prefix', pattern: 'pub/' } },
      'e1',
    );
    expect(await accessRules.getBlockPublic()).toEqual(block);
    await accessRules.setBlockPublic({ enabled: true, mode: 'ignore' });
    await accessRules.setBlockPublic({ enabled: false }, 'b');
    expect(calls).toEqual([
      'test {"keys":["a.txt"],"principal":"anonymous"}',
      'preset {"preset":"public-prefix","match":{"type":"prefix","pattern":"pub/"}} "e1"',
      'cluster {"enabled":true,"mode":"ignore"}',
      'bucket {"enabled":false}',
    ]);
  });

  it('manages the distributions, the routing and the signed URLs', async () => {
    const d: Distribution = {
      id: 'site',
      aliases: ['www.example.com'],
      origin: { bucket: 'web' },
      etag: 'e1',
    };
    const calls: string[] = [];
    server.use(
      http.get(api('/distributions'), () => HttpResponse.json({ items: [d] })),
      http.post(api('/distributions'), () => HttpResponse.json(d, { status: 201 })),
      http.put(api('/distributions/site'), ({ request }) => {
        calls.push(`update ${request.headers.get('If-Match') ?? ''}`);
        return HttpResponse.json(d);
      }),
      http.delete(api('/distributions/site'), () => new HttpResponse(null, { status: 204 })),
      http.post(api('/distributions/site/test'), async ({ request }) => {
        calls.push(`test ${await request.text()}`);
        return HttpResponse.json({
          route: { kind: 'distribution', host: 'www.example.com' },
          behaviorIndex: -1,
          behavior: {},
          responseHeaders: {},
        });
      }),
      http.post(api('/distributions/site/origin-secret/rotate'), async ({ request }) => {
        calls.push(`rotate ${await request.text()}`);
        return HttpResponse.json({ secret: 's3cr3t', header: 'X-Origin-Secret', distribution: d });
      }),
      http.post(api('/distributions/site/sign'), () =>
        HttpResponse.json({
          url: 'https://www.example.com/a?Signature=x',
          cookies: [],
          keyGroup: 'g',
          keyPairId: 'K',
          expiresAt: '2026-10-09T00:00:00Z',
        }),
      ),
      http.get(api('/routing/resolve'), ({ request }) =>
        HttpResponse.json({
          kind: 'path-style',
          host: new URL(request.url).searchParams.get('host'),
        }),
      ),
    );
    const { distributions } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await distributions.list()).toEqual([d]);
    expect(await distributions.create(d)).toEqual(d);
    await distributions.update(d, 'e1');
    await distributions.remove('site');
    await distributions.test('site', { url: '/assets/app.js' });
    expect((await distributions.rotateOriginSecret('site')).secret).toBe('s3cr3t');
    expect(
      (await distributions.sign('site', { url: '/a', privateKeyPem: 'k', expiresIn: 60 }))
        .keyPairId,
    ).toBe('K');
    expect(await distributions.resolve('cdn.example.com:9000')).toEqual({
      kind: 'path-style',
      host: 'cdn.example.com:9000',
    });
    expect(calls).toEqual(['update "e1"', 'test {"url":"/assets/app.js"}', 'rotate {}']);
  });

  it('reads the traffic of the aliases from the cache statistics, or nothing', async () => {
    server.use(
      http.get(api('/cache/stats'), ({ request }) =>
        new URL(request.url).searchParams.get('distribution') === 'site'
          ? HttpResponse.json({
              hosts: [
                {
                  distribution: 'site',
                  host: 'www.example.com',
                  requests: { HIT: 8, MISS: 2 },
                  bytesCache: 100,
                  bytesOrigin: 50,
                  hitRatio: 0.8,
                },
              ],
            })
          : problemResponse(404, 'not_found'),
      ),
    );
    const { distributions } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await distributions.traffic('site')).toEqual({
      hosts: [
        { distribution: 'site', host: 'www.example.com', requests: 10, bytes: 150, hitRatio: 0.8 },
      ],
    });
    expect(await distributions.traffic()).toBeNull();
    expect(trafficOf('nope')).toBeNull();
    expect(trafficOf({ hosts: 'x' })).toBeNull();
  });

  it('manages the key groups and the certificates', async () => {
    const group = { id: 'g', keys: [], createdAt: 'x', updatedAt: 'x' };
    const calls: string[] = [];
    server.use(
      http.get(api('/keygroups'), () => HttpResponse.json({ items: [group] })),
      http.post(api('/keygroups'), () => HttpResponse.json({ keyGroup: group }, { status: 201 })),
      http.get(api('/keygroups/g/keys'), () => HttpResponse.json({ items: [] })),
      http.post(api('/keygroups/g/keys'), async ({ request }) => {
        calls.push(`add ${await request.text()}`);
        return HttpResponse.json({ publicKey: { id: 'K' }, privateKeyPem: 'pem' }, { status: 201 });
      }),
      http.delete(api('/keygroups/g/keys/K%2F1'), () => {
        calls.push('remove key');
        return new HttpResponse(null, { status: 204 });
      }),
      http.delete(api('/keygroups/g'), () => new HttpResponse(null, { status: 204 })),
      http.get(api('/certificates'), () =>
        HttpResponse.json({
          items: [],
          acme: { directory: '', challenge: 'http-01', managed: [] },
        }),
      ),
      http.post(api('/certificates'), () => problemResponse(400, 'invalid_certificate', 'no key')),
      http.delete(api('/certificates/c1'), () => new HttpResponse(null, { status: 204 })),
    );
    const { keyGroups, certificates } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await keyGroups.list()).toEqual([group]);
    expect((await keyGroups.create({ id: 'g' })).keyGroup).toEqual(group);
    expect(await keyGroups.keys('g')).toEqual([]);
    expect((await keyGroups.addKey('g', { algorithm: 'ed25519' })).privateKeyPem).toBe('pem');
    await keyGroups.removeKey('g', 'K/1');
    await keyGroups.remove('g');
    expect((await certificates.list()).acme.challenge).toBe('http-01');
    const failed = (await certificates
      .upload({ certificatePem: 'x', privateKeyPem: 'y' })
      .catch((e: unknown) => e)) as ApiError;
    expect(failed.code).toBe('invalid_certificate');
    await certificates.remove('c1');
    expect(calls).toEqual(['add {"algorithm":"ed25519"}', 'remove key']);
  });
});
