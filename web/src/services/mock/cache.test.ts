import { describe, expect, it } from 'vitest';

import { isApiError } from '../errors';
import { createMockServices } from './index';

async function failure(p: Promise<unknown>) {
  const e = await p.then(
    () => undefined,
    (err: unknown) => err,
  );
  if (!isApiError(e)) throw new Error(`expected an ApiError, got ${String(e)}`);
  return e;
}

function admin(options: Parameters<typeof createMockServices>[0] = {}) {
  return createMockServices({ initialAccessKey: 'nexoadmin', ...options });
}

/** The in-memory cache service answers like the Admin API (docs/11 §11.11). */
describe('mock cache service', () => {
  it('keeps the built-in policies read-only and validates the defined ones', async () => {
    const { cache } = admin();
    const names = (await cache.listPolicies()).map((p) => p.name);
    expect(names).toEqual([
      'aggressive',
      'disabled',
      'immutable-assets',
      'respect-origin',
      'standard',
    ]);
    expect((await failure(cache.createPolicy({ name: 'standard' }))).code).toBe(
      'builtin_cache_policy',
    );
    expect(
      (await failure(cache.createPolicy({ name: 'bad', ttl: { default: '1h', max: '1m' } }))).code,
    ).toBe('invalid_cache_request');
    const created = await cache.createPolicy({ name: 'short', ttl: { default: '10m' } });
    expect(created).toMatchObject({
      builtin: false,
      ttl: { default: '10m', min: '0s', max: '7d' },
    });
    expect((await failure(cache.createPolicy({ name: 'short' }))).code).toBe('already_exists');
    const stale = await failure(cache.updatePolicy({ ...created, comment: 'x' }, 'old'));
    expect(stale.isPreconditionFailed).toBe(true);
    const updated = await cache.updatePolicy({ ...created, comment: 'x' }, created.etag);
    expect(updated.etag).not.toBe(created.etag);
    const preview = await cache.previewPolicy({
      name: '',
      browserCacheControl: 'public, max-age=60',
    });
    expect(preview).toMatchObject({ cacheControl: 'public, max-age=60', inherited: false });
  });

  it('refuses to delete a policy in use, then deletes it', async () => {
    const { cache } = admin({ cachePolicies: [{ name: 'short', ttl: { default: '10m' } }] });
    const cfg = await cache.getDistribution('sito-web');
    await cache.updateDistribution({
      ...cfg,
      domains: [...(cfg.domains ?? []), { host: 'example.com', cache: { policy: 'short' } }],
    });
    expect((await failure(cache.deletePolicy('short'))).code).toBe('cache_policy_in_use');
    const now = await cache.getDistribution('sito-web');
    await cache.updateDistribution({
      ...now,
      domains: (now.domains ?? []).filter((d) => d.host !== 'example.com'),
    });
    await cache.deletePolicy('short');
    expect((await failure(cache.getPolicy('short'))).status).toBe(404);
  });

  it('validates the settings of a distribution: domains, levels, webhooks', async () => {
    const { cache } = admin();
    const cfg = await cache.getDistribution('sito-web');
    const bad = (domains: NonNullable<typeof cfg.domains>) =>
      failure(cache.updateDistribution({ ...cfg, domains }));
    expect((await bad([{ host: 'other.test', cache: { enabled: false } }])).detail).toContain(
      'is not a domain of sito-web',
    );
    expect((await bad([{ host: 'example.com', cache: { policy: 'nope' } }])).detail).toContain(
      'does not exist',
    );
    expect(
      (await bad([{ host: 'example.com', cache: { autoInvalidate: false } }])).detail,
    ).toContain('autoInvalidate is a setting of the distribution');
    // A host below a wildcard alias is a domain of the distribution.
    const ok = await cache.updateDistribution({
      ...cfg,
      domains: [
        ...(cfg.domains ?? []),
        { host: 'PR-7.preview.example.com', cache: { quota: '1GiB' } },
      ],
    });
    expect(ok.domains?.map((d) => d.host)).toContain('pr-7.preview.example.com');
    expect(ok.etag).not.toBe(cfg.etag);
    expect((await failure(cache.updateDistribution(ok, cfg.etag))).isPreconditionFailed).toBe(true);
  });

  it('masks the secrets of the webhooks and keeps the stored ones', async () => {
    const { cache } = admin();
    const listed = await cache.webhooks('sito-web');
    expect(listed[0]).not.toHaveProperty('token');
    // The masked webhook saved back keeps its token...
    const cfg = await cache.getDistribution('sito-web');
    await cache.updateDistribution(cfg, cfg.etag);
    expect((await cache.testWebhook('sito-web', 'cloudflare-www')).ok).toBe(true);
    // ...a new one needs its own.
    expect(
      (await failure(cache.addWebhook('sito-web', { id: 'cf2', type: 'cloudflare', zone: 'z' })))
        .detail,
    ).toContain('needs token');
    await cache.addWebhook('sito-web', {
      id: 'edge',
      type: 'generic',
      url: 'http://127.0.0.1:9/x',
    });
    expect(await cache.testWebhook('sito-web', 'edge')).toMatchObject({ ok: false });
    await cache.removeWebhook('sito-web', 'edge');
    expect((await failure(cache.testWebhook('sito-web', 'edge'))).code).toBe('webhook_not_found');
  });

  it('completes an invalidation node by node as it is read, and drops the entries', async () => {
    const { cache } = admin({ cacheSteps: 1 });
    await cache.warm('download', { paths: ['/db/2026-10-01.sql.gz'] });
    const before = await cache.inspect({
      url: 'https://download.example.com/db/2026-10-01.sql.gz',
    });
    expect(before.nodes[0]?.entry.present).toBe(true);
    const inv = await cache.invalidate('download', {
      patterns: [{ type: 'glob', pattern: '/db/**' }],
    });
    expect(inv.state).toBe('in-progress');
    expect(inv.nodes.map((n) => n.state)).toEqual(['completed', 'pending', 'pending', 'pending']);
    expect(inv.nodes[0]?.removed).toBe(1);
    let read = inv;
    for (let i = 0; i < 3; i += 1) read = await cache.invalidation('download', inv.id);
    expect(read.state).toBe('completed');
    // Found by its id alone too.
    expect((await cache.invalidation('', inv.id)).id).toBe(inv.id);
    expect((await cache.invalidations('download'))[0]?.id).toBe(inv.id);
    const after = await cache.inspect({ url: 'https://download.example.com/db/2026-10-01.sql.gz' });
    expect(after.nodes[0]?.entry.present).toBe(false);
  });

  it('checks the requests of the invalidations', async () => {
    const { cache } = admin();
    expect((await failure(cache.invalidate('sito-web', {}))).code).toBe('invalid_cache_request');
    expect((await failure(cache.invalidate('', { paths: ['/'] }))).detail).toContain(
      'all is required',
    );
    expect(
      (await failure(cache.invalidate('sito-web', { domain: 'other.test', all: false }))).detail,
    ).toContain('is not a domain');
    expect(
      (await failure(cache.invalidate('sito-web', { patterns: [{ type: 'regex', pattern: '(' }] })))
        .code,
    ).toBe('invalid_cache_request');
    expect((await cache.invalidate('', { all: true })).request.distribution).toBeUndefined();
  });

  it('authorizes the configuration with ManageCache and the purges with InvalidateCache', async () => {
    const services = createMockServices({
      accounts: { nexoadmin: 'nexoadmin', ci: 'ci-secret-1' },
      policies: [
        {
          name: 'purge',
          document: {
            Version: '2012-10-17',
            Statement: [{ Effect: 'Allow', Action: ['admin:InvalidateCache'], Resource: ['*'] }],
          },
          builtIn: false,
          updatedAt: '2026-09-01T00:00:00Z',
        },
      ],
      users: [
        {
          accessKey: 'ci',
          status: 'enabled',
          policyNames: ['purge'],
          groups: [],
          createdAt: '2026-09-01T00:00:00Z',
          updatedAt: '2026-09-01T00:00:00Z',
        },
      ],
      groups: [],
      initialAccessKey: 'ci',
    });
    const { cache } = services;
    expect((await failure(cache.listPolicies())).status).toBe(403);
    expect((await failure(cache.stats())).status).toBe(403);
    expect((await failure(cache.getDistribution('sito-web'))).status).toBe(403);
    const inv = await cache.invalidate('sito-web', { paths: ['/index.html'] });
    expect(inv.request.actor).toBe('ci');
    const job = await cache.warm('download', { prefix: '/db/' });
    expect(job.nodes[0]?.total).toBe(2);
  });

  it('filters the statistics by distribution and host', async () => {
    const { cache } = admin();
    const all = await cache.stats();
    expect(all.nodes).toHaveLength(4);
    expect(all.hosts).toHaveLength(4);
    const site = await cache.stats({ distribution: 'sito-web' });
    expect(site.hosts.map((h) => h.host)).toEqual([
      'www.example.com',
      'example.com',
      'pr-12.preview.example.com',
    ]);
    const one = await cache.stats({ host: 'download.example.com', top: 1 });
    expect(one.hosts).toHaveLength(1);
    expect(one.topObjects).toHaveLength(1);
    // Every node a share of the cluster.
    const sum = site.nodes.reduce((s, n) => s + (n.requests.HIT ?? 0), 0);
    expect(sum).toBe(site.requests.HIT);
  });
});
