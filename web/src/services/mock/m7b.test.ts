import { describe, expect, it } from 'vitest';

import type { ApiError } from '../errors';
import type { BucketAccessRule } from '../types';
import { createMockServices } from './index';

const admin = () => createMockServices({ initialAccessKey: 'nexoadmin' });

const publicImages: BucketAccessRule = {
  id: 'img',
  priority: 10,
  effect: 'allow',
  principals: ['*'],
  actions: ['read'],
  match: { type: 'regex', pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png)$', caseInsensitive: true },
};
const noPrivate: BucketAccessRule = {
  id: 'no-private',
  priority: 5,
  effect: 'deny',
  principals: ['*'],
  actions: ['all'],
  match: { type: 'glob', pattern: '**/private/**' },
};

describe('mock access rules', () => {
  it('stores the rules with an etag and refuses a stale If-Match', async () => {
    const { accessRules } = admin();
    const first = await accessRules.get('media');
    expect(first.rules).toEqual([]);
    const saved = await accessRules.set('media', [publicImages], first.etag);
    expect(saved.public).toBe(true);
    expect(saved.etag).not.toBe(first.etag);
    const stale = (await accessRules
      .set('media', [], first.etag)
      .catch((e: unknown) => e)) as ApiError;
    expect(stale.isPreconditionFailed).toBe(true);
  });

  it('locates an invalid pattern like the server', async () => {
    const { accessRules } = admin();
    const error = (await accessRules
      .set('media', [{ ...publicImages, match: { type: 'glob', pattern: 'a/[bc' } }])
      .catch((e: unknown) => e)) as ApiError;
    expect(error.code).toBe('invalid_access_rule');
    expect(error.accessRuleErrors).toEqual([
      expect.objectContaining({ rule: 1, id: 'img', field: 'match.pattern', position: 3 }),
    ]);
  });

  it('evaluates deny rules first and reports the determinant rule', async () => {
    const { accessRules } = admin();
    await accessRules.set('media', [publicImages, noPrivate]);
    const result = await accessRules.test('media', {
      keys: ['img/2026/a.JPG', 'img/2026/private/b.png', 'docs/a.pdf'],
    });
    expect(result.results.map((r) => [r.key, r.allowed, r.determinant, r.matched])).toEqual([
      ['img/2026/a.JPG', true, 'img', ['img']],
      ['img/2026/private/b.png', false, 'no-private', ['no-private', 'img']],
      ['docs/a.pdf', false, undefined, []],
    ]);
  });

  it('ignores the rules for everyone while the block public access is on', async () => {
    const services = admin();
    await services.accessRules.set('media', [publicImages]);
    expect((await services.buckets.get('media')).public).toBe(true);
    await services.accessRules.setBlockPublic({ enabled: true }, 'media');
    const result = await services.accessRules.test('media', { keys: ['img/2026/a.png'] });
    expect(result.results[0]?.blockedPublic).toEqual(['img']);
    expect(result.results[0]?.allowed).toBe(false);
    expect((await services.buckets.get('media')).public).toBe(false);
    // The reject mode of the cluster refuses new public rules.
    await services.accessRules.setBlockPublic({ enabled: true, mode: 'reject' });
    const refused = (await services.accessRules
      .set('backups', [publicImages])
      .catch((e: unknown) => e)) as ApiError;
    expect(refused.code).toBe('public_access_blocked');
  });

  it('applies the presets and needs a confirmation for public-write', async () => {
    const { accessRules } = admin();
    const applied = await accessRules.applyPreset('media', {
      preset: 'public-prefix',
      match: { type: 'prefix', pattern: 'pub/' },
    });
    expect(applied.rules.map((r) => r.id)).toEqual(['preset-public-prefix']);
    expect((await accessRules.publicStatus('media')).preset).toBe('public-prefix');
    const refused = (await accessRules
      .applyPreset('media', { preset: 'public-write' })
      .catch((e: unknown) => e)) as ApiError;
    expect(refused.code).toBe('confirmation_required');
    const priv = await accessRules.applyPreset('media', { preset: 'private' });
    expect(priv.rules).toEqual([]);
  });
});

describe('mock distributions', () => {
  it('routes the hosts: exact, most specific wildcard, main, path-style', async () => {
    const { distributions } = admin();
    expect(await distributions.resolve('WWW.example.com:443')).toMatchObject({
      kind: 'distribution',
      distribution: 'website',
      alias: 'www.example.com',
    });
    expect(await distributions.resolve('pr-1.preview.example.com')).toMatchObject({
      kind: 'distribution',
      alias: '*.preview.example.com',
    });
    expect((await distributions.resolve('localhost')).kind).toBe('main');
    expect((await distributions.resolve('unknown.example.org')).kind).toBe('path-style');
  });

  it('refuses an alias served by another distribution with 409', async () => {
    const { distributions } = admin();
    const error = (await distributions
      .create({ id: 'other', aliases: ['www.example.com'], origin: { bucket: 'media' } })
      .catch((e: unknown) => e)) as ApiError;
    expect(error.status).toBe(409);
    expect(error.code).toBe('alias_conflict');
  });

  it('tests a URL: behavior, key under the prefix, refusals and redirects', async () => {
    const { distributions } = admin();
    const d = await distributions.get('website');
    await distributions.update({ ...d, restrictions: {} }, d.etag);
    const assets = await distributions.test('website', {
      url: 'https://www.example.com/assets/app.js',
    });
    expect(assets.behaviorIndex).toBe(0);
    expect(assets.key).toBe('prod/assets/app.js');
    expect(assets.behavior.cache?.cacheControl).toBe('public, max-age=31536000, immutable');
    const internal = await distributions.test('website', { url: '/internal/x' });
    expect(internal.refusalStatus).toBe(403);
    const post = await distributions.test('website', { url: '/a', method: 'POST' });
    expect(post.refusalStatus).toBe(405);
    const moved = await distributions.test('website', { url: '/old/page' });
    expect(moved.redirectLocation).toBe('/new/page');
  });

  it('rotates the origin secret, shown once and stored hashed', async () => {
    const { distributions } = admin();
    const rotated = await distributions.rotateOriginSecret('website');
    expect(rotated.secret).toHaveLength(40);
    const values = rotated.distribution.restrictions?.originSecretHeader?.values ?? [];
    expect(values).toHaveLength(2);
    expect(values.join()).not.toContain(rotated.secret);
  });

  it('generates key pairs whose private key is returned once', async () => {
    const { keyGroups } = admin();
    const created = await keyGroups.create({
      id: 'premium',
      generateKey: { algorithm: 'ed25519' },
    });
    expect(created.key?.privateKeyPem).toContain('PRIVATE KEY');
    const stored = await keyGroups.get('premium');
    expect(JSON.stringify(stored)).not.toContain('PRIVATE KEY');
  });

  it('uploads certificates and refuses the invalid ones', async () => {
    const { certificates } = admin();
    const bad = (await certificates
      .upload({ certificatePem: 'nope', privateKeyPem: 'nope' })
      .catch((e: unknown) => e)) as ApiError;
    expect(bad.code).toBe('invalid_certificate');
    const list = await certificates.list();
    expect(list.items[0]?.status).toBe('expiring');
    expect(list.items[0]?.usedBy).toEqual(['website']);
  });
});
