import {
  builtinPolicies,
  cacheResults,
  formatSize,
  hostMatches,
  isBuiltinPolicy,
  isDomainOf,
  isValidPolicyName,
  issueText,
  normalizeCacheHost,
  parseDuration,
  parseSize,
  resolveLevels,
  specOf,
  validatePolicy,
  validateSpec,
  type LevelSettings,
} from '@/lib/cache';
import { behaviorFor } from '@/lib/distributions';
import { compileKeyPattern } from '@/lib/keyPattern';

import type { CacheService } from '../interfaces';
import type {
  CacheEffectivePolicy,
  CacheGlobalSettings,
  CacheHostStats,
  CacheInspectionNode,
  CacheInvalidation,
  CacheInvalidationRequest,
  CacheNodeStats,
  CacheObjectStat,
  CachePolicy,
  CacheSettings,
  CacheStats,
  CacheWarmJob,
  CacheWebhook,
  Distribution,
  DistributionCache,
  KeyPattern,
} from '../types';
import type { MockDistributionState } from './distributions';
import { clone, problem, type MockContext } from './support';

export interface MockCacheOptions {
  /** The defined policies (the built-in ones are always there). */
  cachePolicies?: CachePolicy[];
  /** The settings of the cluster. */
  cacheSettings?: CacheGlobalSettings;
  /** The settings of the distributions. */
  cacheDistributions?: DistributionCache[];
  /** The statistics (samples with the sample distributions, empty otherwise). */
  cacheStats?: CacheStats;
  /** The nodes of the cluster (one "local" node, four with the samples). */
  cacheNodes?: string[];
  /**
   * Reads of an invalidation or a warm-up before one more node completes
   * it (1 by default: the console sees the nodes complete one by one; 0
   * completes at once).
   */
  cacheSteps?: number;
}

/** What the cache mock needs from the rest of the bundle. */
export interface MockCacheDeps {
  dist: MockDistributionState;
  /** Access key of the caller, recorded as the actor of the operations. */
  actor: () => string;
  /** The keys of a bucket (latest versions) and their sizes. */
  keysOf: (bucket: string) => { key: string; size: number }[];
}

interface CachedEntry {
  distribution: string;
  host: string;
  path: string;
  tier: 'memory' | 'disk';
  size: number;
  storedAt: number;
  ttl: number;
  stale: boolean;
  tags: string[];
}

interface Progress {
  /** Reads until the next node completes. */
  wait: number;
}

const builtinTime = '0001-01-01T00:00:00Z';

/** The sample cache configuration of the sample distributions (docs/11 §11.1). */
export function sampleCacheConfig(): DistributionCache[] {
  return [
    {
      distribution: 'sito-web',
      cache: { enabled: true, policy: 'standard' },
      domains: [
        { host: 'www.example.com', cache: { policy: 'aggressive', quota: '20GiB' } },
        { host: '*.preview.example.com', cache: { enabled: false } },
      ],
      behaviors: [
        {
          pathPattern: { type: 'glob', pattern: '/assets/**' },
          cache: {
            ttl: { default: '365d', min: '1d', max: '365d' },
            browserCacheControl: 'public, max-age=31536000, immutable',
          },
        },
        { pathPattern: { type: 'regex', pattern: '^/api/' }, cache: { enabled: false } },
      ],
      webhooks: [
        { id: 'cloudflare-www', type: 'cloudflare', zone: 'f1d2', token: 'cf-token', domains: [] },
      ],
      warmOnWrite: [{ type: 'glob', pattern: '/index.html' }],
    },
  ];
}

function counters(requests: Record<string, number>, bytes: number, hitShare = 0.6) {
  const hits = (requests.HIT ?? 0) + (requests.STALE ?? 0) + (requests.REVALIDATED ?? 0);
  const cacheable = hits + (requests.MISS ?? 0) + (requests.EXPIRED ?? 0);
  return {
    requests,
    bytesCache: Math.round(bytes * hitShare),
    bytesOrigin: Math.round(bytes * (1 - hitShare)),
    hitRatio: cacheable === 0 ? 0 : hits / cacheable,
  };
}

/** Sample statistics of the sample distributions, spread over the nodes. */
export function sampleCacheStats(nodes: readonly string[]): CacheStats {
  const hosts: CacheHostStats[] = [
    {
      distribution: 'sito-web',
      host: 'www.example.com',
      ...counters(
        { HIT: 158_200, MISS: 9_800, STALE: 2_100, REVALIDATED: 640, EXPIRED: 300 },
        9.1e9,
        0.93,
      ),
      entries: 1_840,
      memoryBytes: 412_000_000,
      diskBytes: 6_200_000_000,
    },
    {
      distribution: 'sito-web',
      host: 'example.com',
      ...counters({ HIT: 1_500, MISS: 2_200, BYPASS: 510 }, 2.1e7, 0.4),
      entries: 120,
      memoryBytes: 18_000_000,
      diskBytes: 0,
    },
    {
      distribution: 'sito-web',
      host: 'pr-12.preview.example.com',
      ...counters({ BYPASS: 3_400 }, 4.2e7, 0),
      entries: 0,
      memoryBytes: 0,
      diskBytes: 0,
    },
    {
      distribution: 'download',
      host: 'download.example.com',
      ...counters({ HIT: 850, MISS: 300, STALE: 54 }, 6.4e10, 0.71),
      entries: 42,
      memoryBytes: 64_000_000,
      diskBytes: 21_000_000_000,
    },
  ];
  const top: CacheObjectStat[] = [
    {
      distribution: 'sito-web',
      host: 'www.example.com',
      path: '/index.html',
      requests: 48_210,
      misses: 310,
    },
    {
      distribution: 'sito-web',
      host: 'www.example.com',
      path: '/assets/app.js',
      requests: 41_005,
      misses: 12,
    },
    {
      distribution: 'sito-web',
      host: 'www.example.com',
      path: '/assets/app.css',
      requests: 40_870,
      misses: 9,
    },
    { distribution: 'sito-web', host: 'example.com', path: '/', requests: 3_900, misses: 2_100 },
    {
      distribution: 'download',
      host: 'download.example.com',
      path: '/downloads/v1.zip',
      requests: 980,
      misses: 220,
    },
  ];
  return aggregate(hosts, top, nodes);
}

function sumRequests(
  list: readonly { requests: Record<string, number> }[],
): Record<string, number> {
  const out: Record<string, number> = {};
  for (const item of list) {
    for (const [k, v] of Object.entries(item.requests)) out[k] = (out[k] ?? 0) + v;
  }
  return out;
}

function ratioOf(requests: Record<string, number>): number {
  const hits = (requests.HIT ?? 0) + (requests.STALE ?? 0) + (requests.REVALIDATED ?? 0);
  const cacheable = hits + (requests.MISS ?? 0) + (requests.EXPIRED ?? 0);
  return cacheable === 0 ? 0 : hits / cacheable;
}

/** The statistics of the cluster from the per-domain ones, every node a share of them. */
function aggregate(
  hosts: CacheHostStats[],
  top: CacheObjectStat[],
  nodes: readonly string[],
): CacheStats {
  const share = (n: number, i: number) => {
    const base = Math.floor(n / nodes.length);
    return i === 0 ? n - base * (nodes.length - 1) : base;
  };
  const sum = (pick: (h: CacheHostStats) => number) => hosts.reduce((s, h) => s + pick(h), 0);
  const requests = sumRequests(hosts);
  const nodeStats: CacheNodeStats[] = nodes.map((node, i) => {
    const nodeHosts: CacheHostStats[] = hosts.map((h) => ({
      ...h,
      requests: Object.fromEntries(Object.entries(h.requests).map(([k, v]) => [k, share(v, i)])),
      bytesCache: share(h.bytesCache, i),
      bytesOrigin: share(h.bytesOrigin, i),
      entries: share(h.entries, i),
      memoryBytes: share(h.memoryBytes, i),
      diskBytes: share(h.diskBytes, i),
    }));
    const nodeRequests = sumRequests(nodeHosts);
    return {
      node,
      online: true,
      mode: 'local',
      requests: nodeRequests,
      bytesCache: nodeHosts.reduce((s, h) => s + h.bytesCache, 0),
      bytesOrigin: nodeHosts.reduce((s, h) => s + h.bytesOrigin, 0),
      hitRatio: ratioOf(nodeRequests),
      entries: nodeHosts.reduce((s, h) => s + h.entries, 0),
      memoryBytes: nodeHosts.reduce((s, h) => s + h.memoryBytes, 0),
      memoryLimitBytes: 256 * 2 ** 20,
      diskBytes: nodeHosts.reduce((s, h) => s + h.diskBytes, 0),
      diskLimitBytes: hosts.length === 0 ? 0 : 10 * 2 ** 30,
      evictions: share(sum((h) => h.entries) * 3, i),
      invalidations: share(sum((h) => h.entries) * 2, i),
      flushes: 0,
      synced: true,
      originReads: share(
        sum((h) => (h.requests.MISS ?? 0) + (h.requests.BYPASS ?? 0)),
        i,
      ),
      webhookQueue: 0,
      hosts: nodeHosts,
      topObjects: top,
      topMisses: [...top].sort((a, b) => b.misses - a.misses),
    };
  });
  return {
    requests,
    bytesCache: sum((h) => h.bytesCache),
    bytesOrigin: sum((h) => h.bytesOrigin),
    hitRatio: ratioOf(requests),
    entries: sum((h) => h.entries),
    memoryBytes: sum((h) => h.memoryBytes),
    diskBytes: sum((h) => h.diskBytes),
    hosts,
    topObjects: [...top].sort((a, b) => b.requests - a.requests),
    topMisses: [...top].filter((o) => o.misses > 0).sort((a, b) => b.misses - a.misses),
    nodes: nodeStats,
  };
}

const invalid = (detail: string) =>
  problem(400, 'invalid_cache_request', `invalid cache configuration: ${detail}`);

const notFound = (code: string, detail: string) => problem(404, code, detail);

let ids = 0;
function newId(prefix: string): string {
  ids += 1;
  return `${prefix}-${Date.now().toString(16)}${ids.toString(16).padStart(4, '0')}`;
}

function etag(kind: string, version: number): string {
  return `${kind.length.toString(16)}${version.toString(16).padStart(6, '0')}`;
}

/** The webhook without its secrets (masked as absent). */
function maskWebhook(w: CacheWebhook): CacheWebhook {
  const { token: _token, clientSecret: _clientSecret, ...rest } = w;
  return rest;
}

function validateWebhook(w: CacheWebhook, d: Distribution): CacheWebhook {
  if (!isValidPolicyName(w.id)) {
    throw invalid(`webhook id "${w.id}": lowercase letters, digits, '.', '_' and '-'`);
  }
  if (!['cloudflare', 'fastly', 'akamai', 'nginx', 'varnish', 'generic'].includes(w.type)) {
    throw invalid(`webhook ${w.id}: unknown type "${String(w.type)}"`);
  }
  const need = (field: string, v: string | undefined) => {
    if ((v ?? '') === '') throw invalid(`webhook ${w.id}: a ${w.type} webhook needs ${field}`);
  };
  if (w.type === 'cloudflare' || w.type === 'fastly') {
    need('zone', w.zone);
    need('token', w.token);
  } else if (w.type === 'akamai') {
    need('url', w.url);
    need('token', w.token);
    need('clientToken', w.clientToken);
    need('clientSecret', w.clientSecret);
  } else {
    need('url', w.url);
  }
  if (w.url !== undefined && w.url !== '' && !/^https?:\/\/[^/]/.test(w.url)) {
    throw invalid(`webhook url "${w.url}" must be an http(s) URL`);
  }
  const domains = (w.domains ?? []).map(normalizeCacheHost);
  for (const h of domains) {
    if (!isDomainOf(d.aliases, h))
      throw invalid(`webhook ${w.id}: "${h}" is not a domain of ${d.id}`);
  }
  return { ...w, ...(w.domains === undefined ? {} : { domains }) };
}

/**
 * The cache service in memory (D-CCH-01..08): the built-in policies and
 * the defined ones, the settings of the cluster and of the distributions
 * of the distribution mocks, validated like the server, the invalidations
 * and the warm-ups completing node by node as the console reads them, the
 * entries cached by the warm-ups and dropped by the invalidations (the
 * inspector), and static statistics.
 */
export function createMockCacheService(
  ctx: MockContext,
  deps: MockCacheDeps,
  options: MockCacheOptions,
  samples: boolean,
): CacheService {
  const { dist } = deps;
  const nodes =
    options.cacheNodes ?? (samples ? ['node-1', 'node-2', 'node-3', 'node-4'] : ['local']);
  const steps = options.cacheSteps ?? 1;
  const manage = () => {
    dist.guard('admin:ManageCache');
  };
  const operate = () => {
    dist.guard('admin:InvalidateCache');
  };

  const policies = new Map<string, CachePolicy>();
  const policyVersions = new Map<string, number>();
  for (const p of builtinPolicies()) policies.set(p.name, { ...p, updatedAt: builtinTime });
  for (const p of options.cachePolicies ?? []) {
    policies.set(p.name, { ...clone(p), builtin: false, etag: etag(p.name, 1) });
    policyVersions.set(p.name, 1);
  }

  let settings: CacheGlobalSettings = clone(options.cacheSettings ?? { cache: {} });
  let settingsVersion = options.cacheSettings === undefined ? 0 : 1;
  if (settingsVersion > 0) settings.etag = etag('settings', settingsVersion);

  const configs = new Map<string, DistributionCache>();
  const configVersions = new Map<string, number>();
  for (const c of options.cacheDistributions ?? (samples ? sampleCacheConfig() : [])) {
    const id = c.distribution ?? '';
    configs.set(id, { ...clone(c), distribution: id, etag: etag(id, 1), updatedAt: ctx.nowIso() });
    configVersions.set(id, 1);
  }

  const invalidations = new Map<string, CacheInvalidation[]>();
  const progress = new Map<string, Progress>();
  const warmJobs = new Map<string, CacheWarmJob>();
  const entries = new Map<string, CachedEntry>();
  const stats =
    options.cacheStats ?? (samples ? sampleCacheStats(nodes) : aggregate([], [], nodes));

  function distribution(id: string): Distribution {
    const d = dist.distributions.get(id);
    if (d === undefined) {
      throw notFound('distribution_not_found', `distribution not found: ${id}`);
    }
    return d;
  }

  function configOf(id: string): DistributionCache {
    return configs.get(id) ?? { distribution: id, cache: {} };
  }

  /** The domain settings of a host: exact first, then the longest wildcard. */
  function domainIndex(cfg: DistributionCache, host: string): number {
    const domains = cfg.domains ?? [];
    const exact = domains.findIndex((d) => d.host === host);
    if (exact >= 0) return exact;
    let best = -1;
    for (const [i, d] of domains.entries()) {
      if (d.host.startsWith('*.') && hostMatches(d.host, host)) {
        if (best < 0 || d.host.length > (domains[best]?.host.length ?? 0)) best = i;
      }
    }
    return best;
  }

  function effectiveOf(id: string, host: string, path: string): CacheEffectivePolicy {
    const cfg = configs.get(id);
    const levels: LevelSettings[] = [{ level: 'global', settings: settings.cache }];
    let domain: string | undefined;
    let behavior = -1;
    let domainQuota: string | number | undefined;
    if (cfg !== undefined) {
      levels.push({ level: 'distribution', settings: cfg.cache });
      const di = domainIndex(cfg, host);
      if (di >= 0) {
        const d = cfg.domains?.[di];
        levels.push({ level: 'domain', settings: d?.cache });
        domain = d?.host;
        domainQuota = d?.cache.quota;
      }
      behavior = behaviorFor(
        {
          id,
          aliases: [],
          origin: { bucket: '' },
          behaviors: (cfg.behaviors ?? []).map((b) => ({ pathPattern: b.pathPattern })),
        },
        path,
      );
      if (behavior >= 0)
        levels.push({ level: 'behavior', settings: cfg.behaviors?.[behavior]?.cache });
    }
    const eff = resolveLevels(levels, policies);
    eff.behavior = behavior;
    if (domain !== undefined) eff.domain = domain;
    if (cfg?.cache.quota !== undefined) eff.distributionQuota = cfg.cache.quota;
    if (domainQuota !== undefined) eff.domainQuota = domainQuota;
    return eff;
  }

  function validateSettingsLevel(
    where: string,
    s: CacheSettings,
    webhooks: ReadonlySet<string>,
  ): void {
    if (s.policy !== undefined && s.policy !== '' && !policies.has(s.policy)) {
      throw invalid(`${where}policy "${s.policy}" does not exist`);
    }
    if (s.quota !== undefined && parseSize(s.quota) === null) {
      throw invalid(`${where}quota: invalid size "${String(s.quota)}"`);
    }
    for (const w of s.purgeWebhooks ?? []) {
      if (!webhooks.has(w)) throw invalid(`${where}purgeWebhooks: webhook "${w}" does not exist`);
    }
    const issue = validateSpec(specOf(s))[0];
    if (issue !== undefined) throw invalid(`${where}${issueText(issue)}`);
  }

  function validateConfig(cfg: DistributionCache, d: Distribution, current: DistributionCache) {
    const webhooks = new Set<string>();
    const stored = new Map((current.webhooks ?? []).map((w) => [w.id, w]));
    const outWebhooks: CacheWebhook[] = [];
    for (const raw of cfg.webhooks ?? []) {
      // A masked (absent) secret keeps the stored one.
      const old = stored.get(raw.id);
      const w = validateWebhook(
        {
          ...raw,
          ...((raw.token ?? '') === '' && old?.token !== undefined ? { token: old.token } : {}),
          ...((raw.clientSecret ?? '') === '' && old?.clientSecret !== undefined
            ? { clientSecret: old.clientSecret }
            : {}),
        },
        d,
      );
      if (webhooks.has(w.id)) throw invalid(`webhook ${w.id} defined twice`);
      webhooks.add(w.id);
      outWebhooks.push(w);
    }
    validateSettingsLevel('cache.', cfg.cache, webhooks);
    if ((cfg.cache.purgeWebhooks ?? []).length > 0) {
      throw invalid('cache.purgeWebhooks is a domain setting (or list the domains of the webhook)');
    }
    const seen = new Set<string>();
    const domains = (cfg.domains ?? []).map((ds, i) => {
      const host = normalizeCacheHost(ds.host);
      if (!isDomainOf(d.aliases, host)) {
        throw invalid(`domains[${String(i)}]: "${host}" is not a domain of ${d.id}`);
      }
      if (seen.has(host)) throw invalid(`domains[${String(i)}]: ${host} listed twice`);
      seen.add(host);
      if (ds.cache.autoInvalidate !== undefined) {
        throw invalid(`domains[${String(i)}]: autoInvalidate is a setting of the distribution`);
      }
      validateSettingsLevel(`domains[${String(i)}].cache.`, ds.cache, webhooks);
      return { ...ds, host };
    });
    for (const [i, b] of (cfg.behaviors ?? []).entries()) {
      if (
        b.cache.quota !== undefined ||
        (b.cache.purgeWebhooks ?? []).length > 0 ||
        b.cache.autoInvalidate !== undefined
      ) {
        throw invalid(
          `behaviors[${String(i)}]: quota, purgeWebhooks and autoInvalidate are not behavior settings`,
        );
      }
      const compiled = compileKeyPattern(b.pathPattern);
      if (!compiled.ok) throw invalid(`behaviors[${String(i)}]: ${compiled.issue.message}`);
      validateSettingsLevel(`behaviors[${String(i)}].cache.`, b.cache, webhooks);
    }
    for (const [i, p] of (cfg.warmOnWrite ?? []).entries()) {
      const compiled = compileKeyPattern(p);
      if (!compiled.ok) throw invalid(`warmOnWrite[${String(i)}]: ${compiled.issue.message}`);
    }
    return {
      ...cfg,
      distribution: d.id,
      ...(cfg.domains === undefined ? {} : { domains }),
      ...(cfg.webhooks === undefined ? {} : { webhooks: outWebhooks }),
    };
  }

  function masked(cfg: DistributionCache): DistributionCache {
    const out = clone(cfg);
    if (out.webhooks !== undefined) out.webhooks = out.webhooks.map(maskWebhook);
    return out;
  }

  function storeConfig(cfg: DistributionCache): DistributionCache {
    const id = cfg.distribution ?? '';
    const version = (configVersions.get(id) ?? 0) + 1;
    configVersions.set(id, version);
    const stored = { ...clone(cfg), etag: etag(id, version), updatedAt: ctx.nowIso() };
    configs.set(id, stored);
    // Entries cached under settings that changed are dropped.
    for (const [key, e] of entries) if (e.distribution === id) entries.delete(key);
    return masked(stored);
  }

  /** One more node completes the operation when its turn comes. */
  function advance<T extends { nodes: { state: string; at?: string }[] }>(
    id: string,
    item: T,
    onComplete: (n: T['nodes'][number]) => void,
  ): boolean {
    const p = progress.get(id);
    if (p === undefined) return false;
    p.wait -= 1;
    if (p.wait > 0) return false;
    p.wait = steps;
    const next = item.nodes.find((n) => n.state === 'pending' || n.state === 'running');
    if (next === undefined) {
      progress.delete(id);
      return true;
    }
    onComplete(next);
    next.at = ctx.nowIso();
    if (!item.nodes.some((n) => n.state === 'pending' || n.state === 'running')) {
      progress.delete(id);
      return true;
    }
    return false;
  }

  function readInvalidation(inv: CacheInvalidation): CacheInvalidation {
    if (
      advance(inv.id, inv, (n) => {
        n.state = 'completed';
      }) ||
      inv.nodes.every((n) => n.state === 'completed')
    ) {
      inv.state = 'completed';
    }
    return clone(inv);
  }

  function matchesRequest(e: CachedEntry, req: CacheInvalidationRequest): boolean {
    if (req.all === true) return true;
    if (req.domain !== undefined && req.domain !== '' && !hostMatches(req.domain, e.host)) {
      return false;
    }
    const paths = req.paths ?? [];
    const patterns = req.patterns ?? [];
    const tags = req.tags ?? [];
    if (paths.length === 0 && patterns.length === 0 && tags.length === 0) return true;
    for (const raw of paths) {
      let path = raw;
      try {
        if (/^https?:\/\//.test(raw)) path = new URL(raw).pathname;
      } catch {
        // not an URL: an exact path
      }
      if (e.path === path || (path.endsWith('/') && e.path === `${path}index.html`)) return true;
    }
    for (const p of patterns) {
      const c = compileKeyPattern(p);
      if (c.ok && c.compiled.match(e.path).matched) return true;
    }
    return tags.some((t) => e.tags.includes(t));
  }

  function checkRequest(d: Distribution | undefined, req: CacheInvalidationRequest) {
    const any =
      req.all === true ||
      (req.domain ?? '') !== '' ||
      (req.paths ?? []).length > 0 ||
      (req.patterns ?? []).length > 0 ||
      (req.tags ?? []).length > 0;
    if (!any) throw invalid('nothing to invalidate: paths, patterns, tags, a domain or all');
    if (d === undefined) {
      if (req.all !== true) throw invalid('a global purge purges everything: all is required');
      return;
    }
    if ((req.domain ?? '') !== '' && !isDomainOf(d.aliases, normalizeCacheHost(req.domain ?? ''))) {
      throw invalid(`"${req.domain ?? ''}" is not a domain of ${d.id}`);
    }
    for (const [i, p] of (req.patterns ?? []).entries()) {
      const c = compileKeyPattern(p);
      if (!c.ok) throw invalid(`patterns[${String(i)}]: ${c.issue.message}`);
    }
  }

  function warmTargets(
    d: Distribution,
    req: CacheWarmJob['request'],
  ): { path: string; size: number }[] {
    const prefix = (d.origin.pathPrefix ?? '').replace(/^\/+/, '').replace(/\/?$/, '/');
    const base = prefix === '/' ? '' : prefix;
    const keys = deps.keysOf(d.origin.bucket);
    const byPath = new Map(
      keys
        .filter((k) => k.key.startsWith(base) && !k.key.endsWith('/'))
        .map((k) => [`/${k.key.slice(base.length)}`, k.size]),
    );
    const out: { path: string; size: number }[] = [];
    for (const raw of [...(req.paths ?? []), ...(req.urls ?? [])]) {
      let path = raw;
      try {
        if (/^https?:\/\//.test(raw)) path = new URL(raw).pathname;
      } catch {
        // an exact path
      }
      out.push({ path, size: byPath.get(path) ?? -1 });
    }
    if (req.prefix !== undefined && req.prefix !== '') {
      for (const [path, size] of byPath) if (path.startsWith(req.prefix)) out.push({ path, size });
    }
    if (req.pattern !== undefined) {
      const c = compileKeyPattern(req.pattern);
      if (c.ok)
        for (const [path, size] of byPath)
          if (c.compiled.match(path).matched) out.push({ path, size });
    }
    return out;
  }

  function readWarm(job: CacheWarmJob): CacheWarmJob {
    const done = advance(job.id, job, (node) => {
      node.state = node.failed > 0 && node.failed === node.total ? 'failed' : 'completed';
      node.done = node.total;
    });
    if (done || job.nodes.every((n) => n.state === 'completed' || n.state === 'failed')) {
      job.state = job.nodes.every((n) => n.state === 'failed') ? 'failed' : 'completed';
    } else {
      for (const n of job.nodes) {
        if (n.state === 'pending') {
          n.state = 'running';
          n.done = Math.min(n.total, Math.ceil(n.total / 2));
        }
      }
    }
    return clone(job);
  }

  function inspectNodes(key: string, present: CachedEntry | undefined): CacheInspectionNode[] {
    const now = ctx.now().getTime();
    return nodes.map((node, i) => {
      const here = present !== undefined && i === 0;
      const age = here ? Math.max(0, Math.round((now - present.storedAt) / 1000)) : 0;
      return {
        node,
        online: true,
        entry: {
          key,
          present: here,
          ...(here ? { tier: present.tier, kind: 'object' as const } : {}),
          ageSeconds: age,
          expiresInSeconds: here ? (present.stale ? -1 : Math.round(present.ttl - age)) : 0,
          ...(here && present.stale ? { stale: true } : {}),
          sizeBytes: here ? present.size : 0,
          encodings: [],
        },
      };
    });
  }

  function filteredStats(distributionId: string, host: string, top: number): CacheStats {
    if (distributionId === '' && host === '') return clone(trimTop(stats, top));
    const keep = (h: { distribution: string; host: string }) =>
      (distributionId === '' || h.distribution === distributionId) &&
      (host === '' || h.host === host);
    return trimTop(aggregate(stats.hosts.filter(keep), stats.topObjects.filter(keep), nodes), top);
  }

  function trimTop(s: CacheStats, top: number): CacheStats {
    return {
      ...s,
      topObjects: s.topObjects.slice(0, top),
      topMisses: s.topMisses.slice(0, top),
      nodes: s.nodes.map((n) => ({
        ...n,
        topObjects: n.topObjects.slice(0, top),
        topMisses: n.topMisses.slice(0, top),
      })),
    };
  }

  return {
    async listPolicies() {
      await ctx.delay();
      manage();
      return [...policies.values()].sort((a, b) => a.name.localeCompare(b.name)).map(clone);
    },
    async getPolicy(name) {
      await ctx.delay();
      manage();
      const p = policies.get(name);
      if (p === undefined)
        throw notFound('cache_policy_not_found', `cache policy not found: ${name}`);
      return clone(p);
    },
    async createPolicy(policy) {
      await ctx.delay();
      manage();
      if (isBuiltinPolicy(policy.name)) {
        throw problem(409, 'builtin_cache_policy', `${policy.name} is a built-in policy`);
      }
      if (policies.has(policy.name)) {
        throw problem(409, 'already_exists', `cache policy ${policy.name} already exists`);
      }
      const issue = validatePolicy(policy)[0];
      if (issue !== undefined) throw invalid(issueText(issue));
      const { etag: _e, updatedAt: _u, builtin: _b, ...body } = policy;
      const stored: CachePolicy = {
        ...builtinComplete(body),
        builtin: false,
        updatedAt: ctx.nowIso(),
        etag: etag(policy.name, 1),
      };
      policyVersions.set(policy.name, 1);
      policies.set(policy.name, stored);
      return clone(stored);
    },
    async updatePolicy(policy, ifMatch) {
      await ctx.delay();
      manage();
      const current = policies.get(policy.name);
      if (current === undefined) {
        throw notFound('cache_policy_not_found', `cache policy not found: ${policy.name}`);
      }
      if (current.builtin === true) {
        throw problem(409, 'builtin_cache_policy', `${policy.name} is a built-in policy`);
      }
      if (ifMatch !== undefined && ifMatch !== '' && ifMatch !== current.etag) {
        throw problem(412, 'precondition_failed', 'the policy changed since it was read');
      }
      const issue = validatePolicy(policy)[0];
      if (issue !== undefined) throw invalid(issueText(issue));
      const version = (policyVersions.get(policy.name) ?? 1) + 1;
      policyVersions.set(policy.name, version);
      const { etag: _e, updatedAt: _u, builtin: _b, ...body } = policy;
      const stored: CachePolicy = {
        ...builtinComplete(body),
        builtin: false,
        updatedAt: ctx.nowIso(),
        etag: etag(policy.name, version),
      };
      policies.set(policy.name, stored);
      return clone(stored);
    },
    async deletePolicy(name) {
      await ctx.delay();
      manage();
      const current = policies.get(name);
      if (current === undefined)
        throw notFound('cache_policy_not_found', `cache policy not found: ${name}`);
      if (current.builtin === true)
        throw problem(409, 'builtin_cache_policy', `${name} is a built-in policy`);
      const used =
        settings.cache.policy === name ||
        [...configs.values()].some(
          (c) =>
            c.cache.policy === name ||
            (c.domains ?? []).some((d) => d.cache.policy === name) ||
            (c.behaviors ?? []).some((b) => b.cache.policy === name),
        );
      if (used) throw problem(409, 'cache_policy_in_use', `cache policy ${name} is in use`);
      policies.delete(name);
    },
    async previewPolicy(policy) {
      await ctx.delay();
      manage();
      const issue = validatePolicy({
        ...policy,
        name: policy.name === '' ? 'preview' : policy.name,
      })[0];
      if (issue !== undefined) throw invalid(issueText(issue));
      const { etag: _e, updatedAt: _u, builtin: _b, ...body } = policy;
      const complete = builtinComplete(body);
      const cacheControl = complete.browserCacheControl ?? '';
      return {
        policy: complete,
        cacheControl,
        inherited: cacheControl === '',
        summary: summaryOf(complete),
      };
    },
    async getSettings() {
      await ctx.delay();
      manage();
      return maskSettings(settings);
    },
    async updateSettings(next, ifMatch) {
      await ctx.delay();
      manage();
      if (ifMatch !== undefined && ifMatch !== '' && ifMatch !== settings.etag) {
        throw problem(412, 'precondition_failed', 'the settings changed since they were read');
      }
      if (next.cache.quota !== undefined || (next.cache.purgeWebhooks ?? []).length > 0) {
        throw invalid('cache: no quota nor webhooks at the level of the cluster');
      }
      validateSettingsLevel('cache.', next.cache, new Set());
      settingsVersion += 1;
      const secret =
        next.debug !== undefined && (next.debug.secret ?? '') === ''
          ? settings.debug?.secret
          : next.debug?.secret;
      const { etag: _e, updatedAt: _u, ...body } = next;
      settings = {
        ...clone(body),
        ...(next.debug !== undefined
          ? { debug: { ...next.debug, ...(secret === undefined ? {} : { secret }) } }
          : {}),
        updatedAt: ctx.nowIso(),
        etag: etag('settings', settingsVersion),
      };
      entries.clear();
      return maskSettings(settings);
    },
    async getDistribution(id) {
      await ctx.delay();
      manage();
      distribution(id);
      return masked(configOf(id));
    },
    async updateDistribution(config, ifMatch) {
      await ctx.delay();
      manage();
      const id = config.distribution ?? '';
      const d = distribution(id);
      const current = configOf(id);
      if (ifMatch !== undefined && ifMatch !== '' && ifMatch !== current.etag) {
        throw problem(
          412,
          'precondition_failed',
          'the cache settings changed since they were read',
        );
      }
      const { etag: _e, updatedAt: _u, ...body } = config;
      return storeConfig(validateConfig(body, d, current));
    },
    async effective(id, query = {}) {
      await ctx.delay();
      manage();
      distribution(id);
      return effectiveOf(id, normalizeCacheHost(query.host ?? ''), query.path ?? '/');
    },
    async inspect(input) {
      await ctx.delay();
      manage();
      let url: URL;
      let d: Distribution | undefined;
      if (input.url.startsWith('/')) {
        d = distribution(input.distribution ?? '');
        const alias = d.aliases.find((a) => !a.startsWith('*.')) ?? d.aliases[0] ?? 'localhost';
        url = new URL(`https://${alias}${input.url}`);
      } else {
        try {
          url = new URL(input.url);
        } catch {
          throw invalid(`"${input.url}" is not an absolute URL nor a path`);
        }
        const host = url.hostname;
        d = [...dist.distributions.values()].find((x) => isDomainOf(x.aliases, host));
        if (d === undefined)
          throw notFound('distribution_not_found', `no distribution serves ${host}`);
        if (
          input.distribution !== undefined &&
          input.distribution !== '' &&
          input.distribution !== d.id
        ) {
          throw invalid(`${host} is not a domain of ${input.distribution}`);
        }
      }
      const host = url.hostname;
      const path = decodeURIComponent(url.pathname);
      const policy = effectiveOf(d.id, host, path);
      const prefix = (d.origin.pathPrefix ?? '').replace(/^\/+|\/+$/g, '');
      const key = `${prefix === '' ? '' : `${prefix}/`}${path.replace(/^\/+/, '')}`;
      const keyHost = policy.policy.cacheKey?.includeHost === false ? `d:${d.id}` : `h:${host}`;
      const cacheKey = `v1|${keyHost}|${d.origin.bucket}/${key}`;
      const entry = entries.get(`${d.id}|${host}|${path}`);
      return {
        url: url.toString(),
        distribution: d.id,
        host,
        path,
        bucket: d.origin.bucket,
        key,
        cacheKey,
        bypass: !policy.enabled,
        ...(policy.enabled ? {} : { reason: 'the cache is disabled for this path' }),
        policy,
        mode: 'local',
        nodes: inspectNodes(cacheKey, entry),
      };
    },
    async stats(query = {}) {
      await ctx.delay();
      manage();
      let distributionId = query.distribution ?? '';
      const host = query.host ?? '';
      if (distributionId === '' && host !== '') {
        distributionId =
          [...dist.distributions.values()].find((x) => isDomainOf(x.aliases, host))?.id ?? '';
      }
      return filteredStats(distributionId, host, Math.min(query.top ?? 10, 100));
    },
    async invalidate(id, request) {
      await ctx.delay();
      operate();
      const d = id === '' ? undefined : distribution(id);
      checkRequest(d, request);
      let removed = 0;
      for (const [key, e] of entries) {
        if (d !== undefined && e.distribution !== d.id) continue;
        if (!matchesRequest(e, request)) continue;
        removed += 1;
        if (request.soft === true) e.stale = true;
        else entries.delete(key);
      }
      const now = ctx.nowIso();
      const inv: CacheInvalidation = {
        id: newId('inv'),
        request: {
          ...clone(request),
          ...(d === undefined ? {} : { distribution: d.id }),
          actor: deps.actor(),
        },
        createdAt: now,
        state: steps === 0 || nodes.length <= 1 ? 'completed' : 'in-progress',
        nodes: nodes.map((node, i) =>
          i === 0 || steps === 0
            ? { node, state: 'completed', at: now, removed: i === 0 ? removed : 0 }
            : { node, state: 'pending' },
        ),
      };
      if (inv.state !== 'completed') progress.set(inv.id, { wait: steps });
      const list = invalidations.get(id) ?? [];
      list.unshift(inv);
      invalidations.set(id, list);
      return clone(inv);
    },
    async invalidations(id) {
      await ctx.delay();
      operate();
      if (id !== '') distribution(id);
      return (invalidations.get(id) ?? []).map(readInvalidation);
    },
    async invalidation(id, invId) {
      await ctx.delay();
      operate();
      const lists = id === '' ? [...invalidations.values()] : [invalidations.get(id) ?? []];
      for (const list of lists) {
        const found = list.find((i) => i.id === invId);
        if (found !== undefined) return readInvalidation(found);
      }
      throw notFound('invalidation_not_found', `invalidation not found: ${invId}`);
    },
    async warm(id, request) {
      await ctx.delay();
      operate();
      const d = distribution(id);
      const domain = normalizeCacheHost(
        request.domain ?? d.aliases.find((a) => !a.startsWith('*.')) ?? '',
      );
      if (!isDomainOf(d.aliases, domain) || domain.startsWith('*.')) {
        throw invalid(`warm: "${domain}" is not a domain of ${d.id}`);
      }
      if (
        (request.paths ?? []).length === 0 &&
        (request.urls ?? []).length === 0 &&
        (request.prefix ?? '') === '' &&
        request.pattern === undefined
      ) {
        throw invalid('warm: paths, urls, a prefix or a pattern are required');
      }
      const targets = warmTargets(d, request);
      const found = targets.filter((t) => t.size >= 0);
      const policy = (path: string) => effectiveOf(d.id, domain, path);
      for (const t of found) {
        const eff = policy(t.path);
        if (!eff.enabled) continue;
        entries.set(`${d.id}|${domain}|${t.path}`, {
          distribution: d.id,
          host: domain,
          path: t.path,
          tier: t.size > 1_048_576 ? 'disk' : 'memory',
          size: t.size,
          storedAt: ctx.now().getTime(),
          ttl: parseDuration(eff.policy.ttl?.default) ?? 3_600,
          stale: false,
          tags: [],
        });
      }
      const bytes = found.reduce((s, t) => s + t.size, 0);
      const failed = targets.length - found.length;
      const job: CacheWarmJob = {
        id: newId('warm'),
        request: {
          ...clone(request),
          domain,
          concurrency: request.concurrency ?? 4,
          bandwidth: request.bandwidth ?? '64MiB',
          distribution: d.id,
          actor: deps.actor(),
        },
        createdAt: ctx.nowIso(),
        state: 'running',
        nodes: nodes.map((node) => ({
          node,
          state: 'pending',
          total: targets.length,
          done: 0,
          failed,
          bytes,
          ...(failed > 0 ? { error: `${String(failed)} objects not found` } : {}),
        })),
      };
      if (steps === 0) {
        for (const n of job.nodes) {
          n.state = 'completed';
          n.done = n.total;
          n.at = ctx.nowIso();
        }
        job.state = 'completed';
      } else {
        progress.set(job.id, { wait: steps });
      }
      warmJobs.set(job.id, job);
      return clone(job);
    },
    async warmJob(id, jobId) {
      await ctx.delay();
      operate();
      distribution(id);
      const job = warmJobs.get(jobId);
      if (job?.request.distribution !== id) {
        throw notFound('warm_job_not_found', `warm-up not found: ${jobId}`);
      }
      return readWarm(job);
    },
    async webhooks(id) {
      await ctx.delay();
      manage();
      distribution(id);
      return (configOf(id).webhooks ?? []).map(maskWebhook);
    },
    async addWebhook(id, webhook) {
      await ctx.delay();
      manage();
      const d = distribution(id);
      const current = configOf(id);
      if ((current.webhooks ?? []).some((w) => w.id === webhook.id)) {
        throw problem(409, 'already_exists', `webhook ${webhook.id} already exists`);
      }
      const next = validateConfig(
        { ...current, webhooks: [...(current.webhooks ?? []), webhook] },
        d,
        current,
      );
      storeConfig(next);
      return maskWebhook(validateWebhook(webhook, d));
    },
    async removeWebhook(id, webhookId) {
      await ctx.delay();
      manage();
      const d = distribution(id);
      const current = configOf(id);
      if (!(current.webhooks ?? []).some((w) => w.id === webhookId)) {
        throw notFound('webhook_not_found', `purge webhook not found: ${webhookId}`);
      }
      const unbind = (s: CacheSettings): CacheSettings => {
        if (s.purgeWebhooks === undefined) return s;
        const rest = s.purgeWebhooks.filter((w) => w !== webhookId);
        const { purgeWebhooks: _p, ...other } = s;
        return rest.length === 0 ? other : { ...other, purgeWebhooks: rest };
      };
      storeConfig(
        validateConfig(
          {
            ...current,
            webhooks: (current.webhooks ?? []).filter((w) => w.id !== webhookId),
            ...(current.domains === undefined
              ? {}
              : { domains: current.domains.map((ds) => ({ ...ds, cache: unbind(ds.cache) })) }),
          },
          d,
          current,
        ),
      );
    },
    async testWebhook(id, webhookId) {
      await ctx.delay();
      manage();
      distribution(id);
      const w = (configOf(id).webhooks ?? []).find((x) => x.id === webhookId);
      if (w === undefined)
        throw notFound('webhook_not_found', `purge webhook not found: ${webhookId}`);
      // An address on an unreachable port fails like a real call would.
      if (
        w.url !== undefined &&
        /:(1|9)\/?/.test(new URL(w.url.replace(/\{\{.*?\}\}/g, 'x')).host.slice(-2))
      ) {
        return { ok: false, error: 'connection refused', durationMs: 3 };
      }
      return { ok: true, status: 200, durationMs: 42 };
    },
  };
}

/** A policy body completed with the standard values. */
function builtinComplete(p: CachePolicy): CachePolicy {
  const { name, comment, ...spec } = p;
  return { name, ...(comment === undefined ? {} : { comment }), ...completeSpecOf(spec) };
}

function completeSpecOf(spec: Omit<CachePolicy, 'name' | 'comment'>) {
  const standard = builtinPolicies().find((b) => b.name === 'standard');
  const base = { ...(standard ?? {}) };
  delete (base as Partial<CachePolicy>).name;
  delete (base as Partial<CachePolicy>).comment;
  delete (base as Partial<CachePolicy>).builtin;
  return overlay(base, spec);
}

function overlay<T extends object>(base: T, over: object): T {
  const out = structuredClone(base) as Record<string, unknown>;
  for (const [k, v] of Object.entries(over)) {
    if (v === undefined) continue;
    const cur = out[k];
    if (
      typeof v === 'object' &&
      !Array.isArray(v) &&
      v !== null &&
      typeof cur === 'object' &&
      cur !== null &&
      !Array.isArray(cur)
    ) {
      out[k] = {
        ...cur,
        ...Object.fromEntries(
          Object.entries(v as Record<string, unknown>).filter(([, x]) => x !== undefined),
        ),
      };
    } else {
      out[k] = structuredClone(v);
    }
  }
  return out as T;
}

/** The summary of a complete policy (internal/service/cache previewOf), in English. */
function summaryOf(p: CachePolicy): string {
  if (p.bypass?.always === true) return 'no caching: every request goes to the origin';
  const parts = [
    `fresh ${String(p.ttl?.default)} (min ${String(p.ttl?.min)}, max ${String(p.ttl?.max)})`,
  ];
  if (p.respectObjectCacheControl === true) parts.push('object Cache-Control respected');
  parts.push(
    `stale-while-revalidate ${String(p.staleWhileRevalidate)}`,
    `stale-if-error ${String(p.staleIfError)}`,
    `404/403 ${String(p.negativeCaching)}`,
  );
  if (p.compression?.enabled === true && (p.compression.algorithms ?? []).length > 0) {
    parts.push(`compression ${(p.compression.algorithms ?? []).join('+')}`);
  }
  if (p.rangeCaching?.enabled === true) {
    parts.push(`blocks of ${formatSize(parseSize(p.rangeCaching.blockSize) ?? 0)}`);
  }
  return parts.join(', ');
}

function maskSettings(s: CacheGlobalSettings): CacheGlobalSettings {
  const out = clone(s);
  if (out.debug?.secret !== undefined && !out.debug.secret.startsWith('${secret:')) {
    const { secret: _s, ...rest } = out.debug;
    out.debug = rest;
  }
  return out;
}

/** The X-Cache values in the order of the charts, for the mocks of the statistics. */
export const mockCacheResults = cacheResults;

/** Patterns of a request as text (for the tests). */
export function patternText(p: KeyPattern): string {
  return `${p.type}:${p.pattern}`;
}
