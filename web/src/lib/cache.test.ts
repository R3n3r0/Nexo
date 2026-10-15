import { describe, expect, it } from 'vitest';

import type { CachePolicy } from '@/services';

import {
  builtinPolicies,
  cacheableRequests,
  cacheControlOf,
  combineHostStats,
  compactDuration,
  domainOverrideIndex,
  fieldValue,
  formatCacheControl,
  formatDuration,
  formatRatio,
  formatSize,
  isDomainOf,
  isEmptySettings,
  normalizeCacheHost,
  parseCacheControl,
  parseDuration,
  parseSize,
  pruneSettings,
  queryStringOf,
  queryStringText,
  resolveLevels,
  standardSpec,
  validatePolicy,
  validateSpec,
} from './cache';

const policies = new Map<string, CachePolicy>(builtinPolicies().map((p) => [p.name, p]));

describe('durations and sizes', () => {
  it.each([
    ['90s', 90],
    ['1h30m', 5_400],
    ['1h0m0s', 3_600],
    ['365d', 365 * 86_400],
    ['1.5d', 1.5 * 86_400],
    ['0', 0],
    ['500ms', 0.5],
    [3_600, 3_600],
    ['-1m', -60],
  ] as const)('parses %s', (value, seconds) => {
    expect(parseDuration(value)).toBe(seconds);
  });

  it.each(['', '1x', 'h', '1 h', '10', 'abc', '1d2h'])('refuses %j', (value) => {
    expect(parseDuration(value)).toBeNull();
  });

  it('writes the compact form of the server', () => {
    expect(formatDuration(86_400 * 7)).toBe('7d');
    expect(formatDuration(5_400)).toBe('1h30m');
    expect(formatDuration(90)).toBe('1m30s');
    expect(formatDuration(0)).toBe('0s');
    expect(formatDuration(0.25)).toBe('250ms');
    expect(compactDuration('1h0m0s')).toBe('1h');
    expect(compactDuration('nope')).toBe('nope');
    expect(compactDuration(undefined)).toBe('');
  });

  it.each([
    ['20GiB', 20 * 2 ** 30],
    ['512MiB', 512 * 2 ** 20],
    ['5MB', 5_000_000],
    ['1K', 1_024],
    ['8m', 8 * 2 ** 20],
    [1_000, 1_000],
    ['0', 0],
  ] as const)('parses the size %s', (value, bytes) => {
    expect(parseSize(value)).toBe(bytes);
  });

  it('refuses invalid sizes and writes the largest binary unit', () => {
    expect(parseSize('-1')).toBeNull();
    expect(parseSize('1 PB')).toBeNull();
    expect(parseSize('GiB')).toBeNull();
    expect(formatSize(20 * 2 ** 30)).toBe('20GiB');
    expect(formatSize(1_536)).toBe('1536');
    expect(formatSize(3 * 2 ** 20)).toBe('3MiB');
  });
});

describe('query string of the key', () => {
  it('reads every form', () => {
    expect(queryStringOf(undefined)).toEqual({ mode: 'none', params: [] });
    expect(queryStringOf('allowlist:v, w')).toEqual({ mode: 'allowlist', params: ['v', 'w'] });
    expect(queryStringOf({ denylist: ['utm_source'] })).toEqual({
      mode: 'denylist',
      params: ['utm_source'],
    });
    expect(queryStringOf({ mode: 'all' })).toEqual({ mode: 'all', params: [] });
    expect(queryStringText({ mode: 'allowlist', params: ['v'] })).toBe('allowlist:v');
    expect(queryStringText('none')).toBe('none');
  });
});

describe('validation (internal/service/cache validateSpec)', () => {
  it('requires min ≤ default ≤ max', () => {
    expect(validateSpec({ ttl: { min: '1h', default: '30m' } })).toEqual([
      {
        field: 'ttl.min',
        code: 'ttlMinDefault',
        values: { min: '1h', default: '30m' },
      },
    ]);
    expect(validateSpec({ ttl: { default: '2d', max: '1d' } })[0]).toMatchObject({
      field: 'ttl.max',
      code: 'ttlDefaultMax',
    });
    expect(validateSpec({ ttl: { min: '2d', max: '1d' } })[0]).toMatchObject({
      code: 'ttlMinMax',
    });
    expect(validateSpec({ ttl: { min: '1h', default: '1h', max: '1h' } })).toEqual([]);
  });

  it('reports invalid durations, sizes, headers and media types on their fields', () => {
    const issues = validateSpec({
      staleIfError: 'soon',
      negativeCaching: '-1s',
      maxObjectSize: 'big',
      cacheKey: { headers: ['Authorization', 'bad header'], cookies: ['ok'] },
      compression: { algorithms: ['gzip', 'gzip', 'lz4' as never], contentTypes: ['text'] },
      rangeCaching: { blockSize: '1KiB' },
      minSize: '2MiB',
      contentTypes: { deny: ['video/*'] },
    });
    expect(issues.map((i) => `${i.field}:${i.code}`)).toEqual([
      'cacheKey.headers:credentialHeader',
      'cacheKey.headers:header',
      'staleIfError:duration',
      'negativeCaching:negative',
      'compression.algorithms:algorithm',
      'compression.algorithms:algorithm',
      'compression.contentTypes:mediaType',
      'maxObjectSize:size',
      'rangeCaching.blockSize:blockSize',
    ]);
  });

  it('validates a policy completed with the standard values', () => {
    expect(validatePolicy({ name: 'Bad Name' })[0]).toMatchObject({ code: 'name' });
    // A min above the standard default of 1h needs a default too.
    expect(validatePolicy({ name: 'p', ttl: { min: '2h' } })[0]).toMatchObject({
      code: 'ttlMinDefault',
    });
    expect(validatePolicy({ name: 'p', ttl: { min: '2h', default: '3h' } })).toEqual([]);
    for (const p of builtinPolicies()) expect(validatePolicy(p), p.name).toEqual([]);
  });
});

describe('inheritance (TCC-01)', () => {
  it('resolves global → distribution → domain → behavior with the level of every setting', () => {
    const eff = resolveLevels(
      [
        { level: 'global', settings: { staleIfError: '2d' } },
        { level: 'distribution', settings: { enabled: true, policy: 'standard', quota: '1GiB' } },
        { level: 'domain', settings: { policy: 'aggressive' } },
        {
          level: 'behavior',
          settings: { ttl: { default: '365d', max: '365d' }, browserCacheControl: 'public' },
        },
      ],
      policies,
    );
    expect(eff).toMatchObject({
      enabled: true,
      enabledLevel: 'distribution',
      policyName: 'aggressive',
      policyLevel: 'domain',
      cacheControl: 'public',
      autoInvalidate: true,
    });
    expect(eff.policy.ttl).toEqual({ default: '365d', min: '1h0m0s', max: '365d' });
    // Naming a policy replaces what the more general levels set.
    expect(eff.policy.staleIfError).toBe('7d');
    expect(eff.fields['staleIfError']).toBe('domain');
    expect(eff.fields['ttl.default']).toBe('behavior');
    expect(eff.fields['ttl.min']).toBe('domain');
  });

  it('turns the cache off with a disabled level or the disabled policy', () => {
    expect(
      resolveLevels(
        [
          { level: 'distribution', settings: {} },
          { level: 'domain', settings: { enabled: false } },
        ],
        policies,
      ),
    ).toMatchObject({ enabled: false, enabledLevel: 'domain' });
    expect(
      resolveLevels([{ level: 'global', settings: { policy: 'disabled' } }], policies).enabled,
    ).toBe(false);
    expect(
      resolveLevels([{ level: 'distribution', settings: { autoInvalidate: false } }], policies)
        .autoInvalidate,
    ).toBe(false);
  });

  it('starts from the standard policy of docs/11 §11.2', () => {
    const eff = resolveLevels([], policies);
    expect(eff.policyName).toBe('standard');
    expect(eff.policy).toEqual(standardSpec());
    expect(Object.values(eff.fields).every((l) => l === 'default')).toBe(true);
  });
});

describe('Cache-Control of the browsers', () => {
  it('parses and formats the directives', () => {
    const d = parseCacheControl('public, max-age=31536000, immutable, x-custom');
    expect(d).toMatchObject({ visibility: 'public', maxAge: 31_536_000, immutable: true });
    expect(d.extra).toEqual(['x-custom']);
    expect(formatCacheControl({ ...d, immutable: false, staleIfError: 60 })).toBe(
      'public, max-age=31536000, stale-if-error=60, x-custom',
    );
    expect(formatCacheControl(parseCacheControl('private, no-store'))).toBe('private, no-store');
    expect(formatCacheControl(parseCacheControl(''))).toBe('');
  });

  it('previews the header the clients receive', () => {
    expect(cacheControlOf({})).toEqual({ value: '', inherited: true });
    const immutable = builtinPolicies().find((p) => p.name === 'immutable-assets');
    expect(cacheControlOf(immutable ?? {})).toEqual({
      value: 'public, max-age=31536000, immutable',
      inherited: false,
    });
  });
});

describe('domains and statistics', () => {
  it('finds the settings of a host: exact first, then the longest wildcard', () => {
    const domains = [
      { host: '*.example.com' },
      { host: '*.preview.example.com' },
      { host: 'a.example.com' },
    ];
    expect(domainOverrideIndex(domains, 'a.example.com')).toBe(2);
    expect(domainOverrideIndex(domains, 'pr-1.preview.example.com')).toBe(1);
    expect(domainOverrideIndex(domains, 'b.example.com')).toBe(0);
    expect(domainOverrideIndex(domains, 'example.com')).toBe(-1);
    expect(isDomainOf(['*.preview.example.com'], 'x.preview.example.com')).toBe(true);
    expect(isDomainOf(['www.example.com'], 'example.com')).toBe(false);
    expect(normalizeCacheHost(' WWW.Example.com:8080. ')).toBe('www.example.com');
  });

  it('sums the statistics of the hosts below a wildcard', () => {
    const host = (name: string, requests: Record<string, number>) => ({
      distribution: 'd',
      host: name,
      requests,
      bytesCache: 10,
      bytesOrigin: 5,
      hitRatio: 0,
      entries: 1,
      memoryBytes: 100,
      diskBytes: 1_000,
    });
    const sum = combineHostStats(
      [
        host('a.preview.example.com', { HIT: 3, MISS: 1 }),
        host('b.preview.example.com', { HIT: 1, BYPASS: 7 }),
        host('www.example.com', { HIT: 100 }),
      ],
      '*.preview.example.com',
    );
    expect(sum).toMatchObject({
      requests: { HIT: 4, MISS: 1, BYPASS: 7 },
      hitRatio: 0.8,
      bytesCache: 20,
      memoryBytes: 200,
      diskBytes: 2_000,
      entries: 2,
    });
    expect(formatRatio(0.8, 12, 'en')).toBe('80%');
    expect(formatRatio(0, 0, 'en')).toBe('—');
    // BYPASS answers are not cacheable: no ratio for a bypassed domain.
    expect(cacheableRequests({ BYPASS: 7, HIT: 2, MISS: 1, EXPIRED: 1 })).toBe(4);
    expect(cacheableRequests({ BYPASS: 7 })).toBe(0);
  });
});

describe('levels', () => {
  it('prunes the inherited fields', () => {
    expect(
      pruneSettings({
        enabled: undefined,
        policy: 'standard',
        ttl: { default: undefined },
        purgeWebhooks: [],
        bypass: { cookies: [] },
      }),
    ).toEqual({ policy: 'standard', bypass: { cookies: [] } });
    expect(isEmptySettings({ ttl: {} })).toBe(true);
    expect(isEmptySettings({ enabled: false })).toBe(false);
  });

  it('shows the value of a dotted field', () => {
    const spec = standardSpec();
    expect(fieldValue(spec, 'ttl.default')).toBe('1h');
    expect(fieldValue(spec, 'cacheKey.queryString')).toBe('none');
    expect(fieldValue(spec, 'compression.algorithms')).toBe('gzip, br');
    expect(fieldValue(spec, 'bypass.cookies')).toBe('—');
    expect(fieldValue(spec, 'respectObjectCacheControl')).toBe('true');
    expect(fieldValue(spec, 'maxObjectSize')).toBe('1GiB');
  });
});
