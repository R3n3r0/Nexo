import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { api, problemResponse } from '@/test/handlers';
import { server } from '@/test/server';

import { ApiError } from '../errors';
import type { CacheInvalidation, CachePolicy, DistributionCache } from '../types';
import { createHttpServices } from './index';

const policy: CachePolicy = {
  name: 'short',
  ttl: { default: '10m', min: '0s', max: '7d' },
  builtin: false,
  etag: 'p1',
  updatedAt: '2026-10-01T00:00:00Z',
};

const invalidation: CacheInvalidation = {
  id: 'inv-1',
  request: { paths: ['/a b'] },
  createdAt: '2026-10-01T00:00:00Z',
  state: 'completed',
  nodes: [{ node: 'n1', state: 'completed' }],
};

/** The cache routes of the Admin API (docs/11 §11.11). */
describe('HTTP cache service', () => {
  it('sends the policies as edited, with If-Match, without the server fields', async () => {
    const seen: { method: string; ifMatch: string | null; body: string }[] = [];
    server.use(
      http.get(api('/cache/policies'), () => HttpResponse.json({ items: [policy] })),
      http.put(api('/cache/policies/short'), async ({ request }) => {
        seen.push({
          method: 'PUT',
          ifMatch: request.headers.get('If-Match'),
          body: await request.text(),
        });
        return HttpResponse.json(policy);
      }),
      http.post(api('/cache/preview'), async ({ request }) => {
        seen.push({ method: 'POST', ifMatch: null, body: await request.text() });
        return HttpResponse.json({
          policy,
          cacheControl: '',
          inherited: true,
          summary: 'fresh 10m',
        });
      }),
      http.delete(api('/cache/policies/a%2Fb'), () => new HttpResponse(null, { status: 204 })),
    );
    const { cache } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await cache.listPolicies()).toEqual([policy]);
    await cache.updatePolicy(policy, 'p1');
    expect((await cache.previewPolicy(policy)).summary).toBe('fresh 10m');
    await cache.deletePolicy('a/b');
    const body = JSON.stringify({ name: 'short', ttl: policy.ttl });
    expect(seen).toEqual([
      { method: 'PUT', ifMatch: '"p1"', body },
      { method: 'POST', ifMatch: null, body },
    ]);
  });

  it('reads and replaces the settings of a distribution', async () => {
    const config: DistributionCache = {
      distribution: 'site',
      cache: { policy: 'standard' },
      etag: 'e1',
      updatedAt: '2026-10-01T00:00:00Z',
    };
    let put = '';
    server.use(
      http.get(api('/distributions/site/cache'), () => HttpResponse.json(config)),
      http.put(api('/distributions/site/cache'), async ({ request }) => {
        put = await request.text();
        return request.headers.get('If-Match') === '"old"'
          ? problemResponse(412, 'precondition_failed', 'changed')
          : HttpResponse.json(config);
      }),
      http.get(api('/distributions/site/cache/effective'), ({ request }) => {
        const url = new URL(request.url);
        return HttpResponse.json({
          enabled: true,
          enabledLevel: 'default',
          policyName: url.searchParams.get('host') ?? '',
          policyLevel: 'default',
          policy: {},
          fields: {},
          behavior: -1,
          autoInvalidate: url.searchParams.get('path') === '/x',
        });
      }),
    );
    const { cache } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await cache.getDistribution('site')).toEqual(config);
    await cache.updateDistribution(config, 'e1');
    expect(JSON.parse(put)).toEqual({ distribution: 'site', cache: { policy: 'standard' } });
    const stale = (await cache
      .updateDistribution(config, 'old')
      .catch((e: unknown) => e)) as ApiError;
    expect(stale).toBeInstanceOf(ApiError);
    expect(stale.isPreconditionFailed).toBe(true);
    expect(await cache.effective('site', { host: 'www.site.test', path: '/x' })).toMatchObject({
      policyName: 'www.site.test',
      autoInvalidate: true,
    });
  });

  it('invalidates a distribution or everything and reads the history', async () => {
    const posted: { path: string; body: string }[] = [];
    server.use(
      http.post(api('/distributions/my%20site/invalidations'), async ({ request }) => {
        posted.push({ path: 'dist', body: await request.text() });
        return HttpResponse.json(invalidation, { status: 202 });
      }),
      http.post(api('/cache/invalidations'), async ({ request }) => {
        posted.push({ path: 'global', body: await request.text() });
        return HttpResponse.json(invalidation, { status: 202 });
      }),
      http.get(api('/distributions/my%20site/invalidations'), () =>
        HttpResponse.json({ items: [invalidation] }),
      ),
      http.get(api('/cache/invalidations/inv-1'), () => HttpResponse.json(invalidation)),
      http.get(api('/distributions/my%20site/invalidations/inv-1'), () =>
        HttpResponse.json(invalidation),
      ),
    );
    const { cache } = createHttpServices({ baseUrl: '/api/v1' });
    await cache.invalidate('my site', {
      domain: 'www.site.test',
      patterns: [{ type: 'glob', pattern: '/assets/**' }],
      soft: true,
    });
    await cache.invalidate('', { all: true });
    expect(posted).toEqual([
      {
        path: 'dist',
        body: JSON.stringify({
          domain: 'www.site.test',
          patterns: [{ type: 'glob', pattern: '/assets/**' }],
          soft: true,
        }),
      },
      { path: 'global', body: JSON.stringify({ all: true }) },
    ]);
    expect(await cache.invalidations('my site')).toEqual([invalidation]);
    expect((await cache.invalidation('', 'inv-1')).id).toBe('inv-1');
    expect((await cache.invalidation('my site', 'inv-1')).id).toBe('inv-1');
  });

  it('passes the inspector and statistics filters as query parameters', async () => {
    const queries: string[] = [];
    server.use(
      http.get(api('/cache/inspect'), ({ request }) => {
        queries.push(new URL(request.url).search);
        return HttpResponse.json({});
      }),
      http.get(api('/cache/stats'), ({ request }) => {
        queries.push(new URL(request.url).search);
        return HttpResponse.json({});
      }),
    );
    const { cache } = createHttpServices({ baseUrl: '/api/v1' });
    await cache.inspect({
      url: '/a b.js',
      distribution: 'site',
      headers: ['Accept-Encoding: br', ' ', 'Accept-Language: it'],
    });
    await cache.inspect({ url: 'https://x.test/', distribution: '' });
    await cache.stats({ distribution: 'site', host: '', top: 5 });
    await cache.stats();
    expect(queries).toEqual([
      '?url=%2Fa+b.js&distribution=site&header=Accept-Encoding%3A+br&header=Accept-Language%3A+it',
      '?url=https%3A%2F%2Fx.test%2F',
      '?distribution=site&top=5',
      '',
    ]);
  });

  it('starts warm-ups and manages the purge webhooks of a distribution', async () => {
    const calls: string[] = [];
    server.use(
      http.post(api('/distributions/site/cache/warm'), async ({ request }) => {
        calls.push(`warm ${await request.text()}`);
        return HttpResponse.json({
          id: 'w 1',
          request: {},
          createdAt: '',
          state: 'running',
          nodes: [],
        });
      }),
      http.get(api('/distributions/site/cache/warm/w%201'), () => {
        calls.push('job');
        return HttpResponse.json({
          id: 'w 1',
          request: {},
          createdAt: '',
          state: 'completed',
          nodes: [],
        });
      }),
      http.get(api('/distributions/site/cache/webhooks'), () =>
        HttpResponse.json({ items: [{ id: 'edge', type: 'generic', url: 'http://x' }] }),
      ),
      http.post(api('/distributions/site/cache/webhooks'), async ({ request }) => {
        calls.push(`add ${await request.text()}`);
        return HttpResponse.json({ id: 'edge', type: 'generic' }, { status: 201 });
      }),
      http.delete(api('/distributions/site/cache/webhooks/edge'), () => {
        calls.push('remove');
        return new HttpResponse(null, { status: 204 });
      }),
      http.post(api('/distributions/site/cache/webhooks/edge/test'), () =>
        HttpResponse.json({ ok: false, error: 'connection refused', durationMs: 3 }),
      ),
    );
    const { cache } = createHttpServices({ baseUrl: '/api/v1' });
    const job = await cache.warm('site', { prefix: '/assets/', concurrency: 2 });
    expect((await cache.warmJob('site', job.id)).state).toBe('completed');
    expect(await cache.webhooks('site')).toHaveLength(1);
    await cache.addWebhook('site', { id: 'edge', type: 'generic', url: 'http://x' });
    expect(await cache.testWebhook('site', 'edge')).toEqual({
      ok: false,
      error: 'connection refused',
      durationMs: 3,
    });
    await cache.removeWebhook('site', 'edge');
    expect(calls).toEqual([
      `warm ${JSON.stringify({ prefix: '/assets/', concurrency: 2 })}`,
      'job',
      `add ${JSON.stringify({ id: 'edge', type: 'generic', url: 'http://x' })}`,
      'remove',
    ]);
  });

  it('saves the settings of the cluster without the server fields', async () => {
    let body = '';
    server.use(
      http.put(api('/cache/settings'), async ({ request }) => {
        body = await request.text();
        return HttpResponse.json({ cache: {}, nodeHeader: true, etag: 'g2' });
      }),
    );
    const { cache } = createHttpServices({ baseUrl: '/api/v1' });
    const saved = await cache.updateSettings(
      { cache: { enabled: true }, nodeHeader: true, etag: 'g1', updatedAt: 'x' },
      'g1',
    );
    expect(saved.etag).toBe('g2');
    expect(JSON.parse(body)).toEqual({ cache: { enabled: true }, nodeHeader: true });
  });
});
