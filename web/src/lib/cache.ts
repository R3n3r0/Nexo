/**
 * The cache of the distributions on the console side (docs/11): the
 * durations and sizes of the configuration ("1h30m", "365d", "20GiB"),
 * the built-in policies, the inheritance global → distribution → domain →
 * behavior, the validation of a policy as the server does it
 * (internal/service/cache/policies.go) for the live feedback of the
 * editors, and the Cache-Control directives of the browser header.
 *
 * The server stays the reference: the editors validate as the user types,
 * the server validates again on save (and on POST /cache/preview).
 */
import type {
  CacheDuration,
  CacheEffectivePolicy,
  CacheHostStats,
  CacheLevel,
  CachePolicy,
  CachePolicySpec,
  CacheQueryString,
  CacheSettings,
  CacheSize,
  CacheCounters,
} from '@/services';

/* ------------------------------------------------------------------------ */
/* Query keys                                                               */
/* ------------------------------------------------------------------------ */

/** TanStack Query keys of the cache pages. */
export const cacheKeys = {
  all: ['cache'] as const,
  policies: ['cache', 'policies'] as const,
  policy: (name: string) => ['cache', 'policies', name] as const,
  settings: ['cache', 'settings'] as const,
  distribution: (id: string) => ['cache', 'distribution', id] as const,
  effective: (id: string, host: string, path: string) =>
    ['cache', 'distribution', id, 'effective', host, path] as const,
  webhooks: (id: string) => ['cache', 'distribution', id, 'webhooks'] as const,
  stats: (distribution: string, host: string) => ['cache', 'stats', distribution, host] as const,
  invalidations: (distribution: string) => ['cache', 'invalidations', distribution] as const,
  invalidation: (distribution: string, id: string) =>
    ['cache', 'invalidations', distribution, id] as const,
  warmJob: (distribution: string, id: string) => ['cache', 'warm', distribution, id] as const,
};

/* ------------------------------------------------------------------------ */
/* Durations and sizes                                                      */
/* ------------------------------------------------------------------------ */

const day = 86_400;

const durationUnits: Readonly<Record<string, number>> = {
  ns: 1e-9,
  us: 1e-6,
  µs: 1e-6,
  μs: 1e-6,
  ms: 1e-3,
  s: 1,
  m: 60,
  h: 3_600,
};

/**
 * Seconds of a duration of the configuration: the Go syntax ("1h30m",
 * "90s", "1h0m0s"), days ("365d", "1.5d") or a number of seconds; null
 * when it is not a duration. Negative durations parse (the validation
 * refuses them).
 */
export function parseDuration(value: CacheDuration | undefined | null): number | null {
  if (typeof value === 'number') {
    return Number.isFinite(value) ? value : null;
  }
  if (typeof value !== 'string') return null;
  const s = value.trim();
  if (s === '') return null;
  if (s.endsWith('d')) {
    const body = s.slice(0, -1);
    if (!/^[-+]?(\d+(\.\d*)?|\.\d+)$/.test(body)) return null;
    return Number(body) * day;
  }
  // time.ParseDuration: [-+]?([0-9]*(\.[0-9]*)?[a-z]+)+, or "0".
  let rest = s;
  let sign = 1;
  if (rest.startsWith('-') || rest.startsWith('+')) {
    if (rest.startsWith('-')) sign = -1;
    rest = rest.slice(1);
  }
  if (rest === '0') return 0;
  if (rest === '') return null;
  let total = 0;
  const part = /^(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/;
  while (rest !== '') {
    const m = part.exec(rest);
    if (m === null) return null;
    total += Number(m[1]) * (durationUnits[m[2] ?? ''] ?? 0);
    rest = rest.slice(m[0].length);
  }
  return sign * total;
}

/**
 * A duration in the compact syntax the server accepts: whole days as
 * "<n>d", otherwise hours, minutes and seconds ("1h30m", "90s" → "1m30s").
 */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds)) return '';
  if (seconds === 0) return '0s';
  const sign = seconds < 0 ? '-' : '';
  let rest = Math.abs(seconds);
  if (rest >= day && rest % day === 0) return `${sign}${String(rest / day)}d`;
  if (rest < 1) return `${sign}${String(Math.round(rest * 1000))}ms`;
  let out = '';
  const h = Math.floor(rest / 3_600);
  if (h > 0) out += `${String(h)}h`;
  rest -= h * 3_600;
  const m = Math.floor(rest / 60);
  if (m > 0) out += `${String(m)}m`;
  rest -= m * 60;
  if (rest > 0) out += `${String(Math.round(rest * 1000) / 1000)}s`;
  return `${sign}${out}`;
}

/** A duration of the server ("1h0m0s") in the compact form ("1h"); as is when invalid. */
export function compactDuration(value: CacheDuration | undefined | null): string {
  const seconds = parseDuration(value);
  if (seconds === null) return value === undefined || value === null ? '' : String(value);
  return formatDuration(seconds);
}

const sizeUnits: readonly (readonly [string, number])[] = [
  ['tib', 2 ** 40],
  ['gib', 2 ** 30],
  ['mib', 2 ** 20],
  ['kib', 2 ** 10],
  ['tb', 1e12],
  ['gb', 1e9],
  ['mb', 1e6],
  ['kb', 1e3],
  ['t', 2 ** 40],
  ['g', 2 ** 30],
  ['m', 2 ** 20],
  ['k', 2 ** 10],
  ['b', 1],
];

/**
 * Bytes of a size of the configuration: a number of bytes, or a number
 * with a unit (KiB, MiB, GiB, TiB, KB, MB, GB, TB; K, M, G, T are binary);
 * null when it is not a size.
 */
export function parseSize(value: CacheSize | undefined | null): number | null {
  if (typeof value === 'number') {
    return Number.isFinite(value) && value >= 0 ? Math.floor(value) : null;
  }
  if (typeof value !== 'string') return null;
  let s = value.trim().toLowerCase();
  if (s === '') return null;
  let mult = 1;
  for (const [suffix, m] of sizeUnits) {
    if (s.endsWith(suffix)) {
      s = s.slice(0, -suffix.length).trim();
      mult = m;
      break;
    }
  }
  if (!/^(\d+(\.\d*)?|\.\d+)$/.test(s)) return null;
  return Math.floor(Number(s) * mult);
}

/** A size in the form the server writes: the largest binary unit dividing it ("20GiB"). */
export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '';
  for (const [unit, mult] of [
    ['TiB', 2 ** 40],
    ['GiB', 2 ** 30],
    ['MiB', 2 ** 20],
    ['KiB', 2 ** 10],
  ] as const) {
    if (bytes !== 0 && bytes % mult === 0) return `${String(bytes / mult)}${unit}`;
  }
  return String(Math.floor(bytes));
}

/* ------------------------------------------------------------------------ */
/* Query string of the key                                                  */
/* ------------------------------------------------------------------------ */

export type QueryStringMode = 'none' | 'all' | 'allowlist' | 'denylist';

export interface QueryStringSetting {
  mode: QueryStringMode;
  params: string[];
}

const queryModes: readonly string[] = ['none', 'all', 'allowlist', 'denylist'];

/** The mode and the parameters of any form of CacheQueryString. */
export function queryStringOf(q: CacheQueryString | undefined): QueryStringSetting {
  if (q === undefined) return { mode: 'none', params: [] };
  if (typeof q === 'string') {
    const [mode = '', list = ''] = q.split(/:(.*)/s);
    const m = mode.trim().toLowerCase();
    return {
      mode: (queryModes.includes(m) ? m : 'none') as QueryStringMode,
      params: list
        .split(',')
        .map((p) => p.trim())
        .filter((p) => p !== ''),
    };
  }
  if (q.allowlist !== undefined) return { mode: 'allowlist', params: [...q.allowlist] };
  if (q.denylist !== undefined) return { mode: 'denylist', params: [...q.denylist] };
  return { mode: q.mode ?? 'none', params: [...(q.params ?? [])] };
}

/** The string form of a query string setting ("allowlist:v,w"). */
export function queryStringText(q: CacheQueryString | undefined): string {
  const { mode, params } = queryStringOf(q);
  return params.length === 0 ? mode : `${mode}:${params.join(',')}`;
}

/* ------------------------------------------------------------------------ */
/* Policies and inheritance                                                 */
/* ------------------------------------------------------------------------ */

/** The media types compressed by default. */
export const textMediaTypes: readonly string[] = [
  'text/*',
  'application/javascript',
  'application/x-javascript',
  'application/json',
  'application/ld+json',
  'application/manifest+json',
  'application/xml',
  'application/xhtml+xml',
  'application/rss+xml',
  'application/atom+xml',
  'application/wasm',
  'image/svg+xml',
  'font/ttf',
  'font/otf',
  'application/vnd.ms-fontobject',
];

/** The complete standard policy (docs/11 §11.2 defaults): every other one starts from it. */
export function standardSpec(): CachePolicySpec {
  return {
    ttl: { default: '1h0m0s', min: '0s', max: '7d' },
    respectObjectCacheControl: true,
    browserCacheControl: '',
    cacheKey: {
      queryString: { mode: 'none' },
      headers: [],
      cookies: [],
      normalize: true,
      includeHost: true,
    },
    staleWhileRevalidate: '1m0s',
    staleIfError: '1d',
    negativeCaching: '10s',
    bypass: { always: false, authorization: true, cookies: [], queryParams: [], headers: [] },
    compression: {
      enabled: true,
      algorithms: ['gzip', 'br'],
      contentTypes: [...textMediaTypes],
      minSize: '1KiB',
    },
    minSize: '0',
    maxObjectSize: '1GiB',
    rangeCaching: { enabled: true, blockSize: '8MiB' },
    contentTypes: { allow: [], deny: [] },
  };
}

/** Names of the built-in policies. */
export const builtinPolicyNames = [
  'disabled',
  'standard',
  'aggressive',
  'immutable-assets',
  'respect-origin',
] as const;

export function isBuiltinPolicy(name: string): boolean {
  return (builtinPolicyNames as readonly string[]).includes(name);
}

/** The built-in policies, complete (internal/service/cache builtinPolicies). */
export function builtinPolicies(): CachePolicy[] {
  const add = (name: string, comment: string, over: CachePolicySpec): CachePolicy => ({
    name,
    comment,
    builtin: true,
    ...completeSpec(over),
  });
  return [
    add('aggressive', 'Long lifetimes (1d, at least 1h, at most 30d), long stale windows, zstd too.', {
      ttl: { default: '1d', min: '1h0m0s', max: '30d' },
      staleWhileRevalidate: '1h0m0s',
      staleIfError: '7d',
      negativeCaching: '1m0s',
      compression: { algorithms: ['br', 'zstd', 'gzip'] },
    }),
    add('disabled', 'No caching: every request is served by the origin.', {
      bypass: { always: true },
    }),
    add('immutable-assets', 'Versioned assets: one year in cache and in the browsers.', {
      ttl: { default: '365d', min: '30d', max: '365d' },
      respectObjectCacheControl: false,
      browserCacheControl: 'public, max-age=31536000, immutable',
      staleWhileRevalidate: '1d',
      staleIfError: '30d',
      negativeCaching: '1m0s',
    }),
    add(
      'respect-origin',
      'Only the lifetime the objects state (Cache-Control, Expires); none means revalidate.',
      { ttl: { default: '0s', min: '0s', max: '365d' }, respectObjectCacheControl: true },
    ),
    add(
      'standard',
      'Default: 1h fresh (bounded to 7d), the Cache-Control of the objects respected.',
      {},
    ),
  ];
}

/** Every field of a policy, for the provenance of the effective policy. */
export const policyFields = [
  'ttl.default',
  'ttl.min',
  'ttl.max',
  'respectObjectCacheControl',
  'browserCacheControl',
  'cacheKey.queryString',
  'cacheKey.headers',
  'cacheKey.cookies',
  'cacheKey.normalize',
  'cacheKey.includeHost',
  'staleWhileRevalidate',
  'staleIfError',
  'negativeCaching',
  'bypass.always',
  'bypass.authorization',
  'bypass.cookies',
  'bypass.queryParams',
  'bypass.headers',
  'compression.enabled',
  'compression.algorithms',
  'compression.contentTypes',
  'compression.minSize',
  'minSize',
  'maxObjectSize',
  'rangeCaching.enabled',
  'rangeCaching.blockSize',
  'contentTypes.allow',
  'contentTypes.deny',
] as const;

type Group = 'ttl' | 'cacheKey' | 'bypass' | 'compression' | 'rangeCaching' | 'contentTypes';
const groups: readonly Group[] = [
  'ttl',
  'cacheKey',
  'bypass',
  'compression',
  'rangeCaching',
  'contentTypes',
];
const scalars = [
  'respectObjectCacheControl',
  'browserCacheControl',
  'staleWhileRevalidate',
  'staleIfError',
  'negativeCaching',
  'minSize',
  'maxObjectSize',
] as const;

/**
 * Copies the set fields of src over dst (a new object is returned);
 * fields, when given, records the level of every field copied.
 */
export function overlaySpec(
  dst: CachePolicySpec,
  src: CachePolicySpec,
  level?: CacheLevel,
  fields?: Record<string, CacheLevel>,
): CachePolicySpec {
  const out: CachePolicySpec = structuredClone(dst);
  const rec = (name: string) => {
    if (fields !== undefined && level !== undefined) fields[name] = level;
  };
  for (const key of scalars) {
    const v = src[key];
    if (v !== undefined) {
      (out as Record<string, unknown>)[key] = structuredClone(v);
      rec(key);
    }
  }
  for (const g of groups) {
    const sub = src[g] as Record<string, unknown> | undefined;
    if (sub === undefined) continue;
    const target = { ...((out[g] as Record<string, unknown> | undefined) ?? {}) };
    for (const [k, v] of Object.entries(sub)) {
      if (v === undefined) continue;
      target[k] = structuredClone(v);
      rec(`${g}.${k}`);
    }
    (out as Record<string, unknown>)[g] = target;
  }
  return out;
}

/** A spec completed with the standard policy (the unset fields take its values). */
export function completeSpec(spec: CachePolicySpec): CachePolicySpec {
  return overlaySpec(standardSpec(), spec);
}

/** The policy fields of settings (without enabled, policy, quota...). */
export function specOf(settings: CacheSettings | undefined): CachePolicySpec {
  if (settings === undefined) return {};
  const {
    enabled: _enabled,
    policy: _policy,
    quota: _quota,
    purgeWebhooks: _purgeWebhooks,
    autoInvalidate: _autoInvalidate,
    ...spec
  } = settings;
  return spec;
}

/** The level settings of a resolution, the most general first. */
export interface LevelSettings {
  level: CacheLevel;
  settings: CacheSettings | undefined;
}

/**
 * The effective policy of the levels (internal/service/cache resolve):
 * the standard policy, then every level: enabled, the policy it names
 * (replacing what the more general levels set), its overrides.
 */
export function resolveLevels(
  levels: readonly LevelSettings[],
  policies: ReadonlyMap<string, CachePolicy>,
): CacheEffectivePolicy {
  let spec = standardSpec();
  const fields: Record<string, CacheLevel> = {};
  for (const f of policyFields) fields[f] = 'default';
  const eff: CacheEffectivePolicy = {
    enabled: true,
    enabledLevel: 'default',
    policyName: 'standard',
    policyLevel: 'default',
    policy: spec,
    fields,
    behavior: -1,
    autoInvalidate: true,
  };
  for (const { level, settings } of levels) {
    if (settings === undefined) continue;
    if (settings.enabled !== undefined) {
      eff.enabled = settings.enabled;
      eff.enabledLevel = level;
    }
    if (settings.policy !== undefined && settings.policy !== '') {
      const p = policies.get(settings.policy);
      if (p !== undefined) {
        spec = completeSpec(specOfPolicy(p));
        for (const f of policyFields) fields[f] = level;
        eff.policyName = settings.policy;
        eff.policyLevel = level;
      }
    }
    spec = overlaySpec(spec, specOf(settings), level, fields);
    if (
      settings.autoInvalidate !== undefined &&
      (level === 'global' || level === 'distribution')
    ) {
      eff.autoInvalidate = settings.autoInvalidate;
    }
  }
  eff.policy = spec;
  eff.cacheControl = spec.browserCacheControl ?? '';
  if (spec.bypass?.always === true) eff.enabled = false;
  return eff;
}

/** The spec of a policy (without its name and metadata). */
export function specOfPolicy(p: CachePolicy): CachePolicySpec {
  const {
    name: _name,
    comment: _comment,
    builtin: _builtin,
    updatedAt: _updatedAt,
    etag: _etag,
    ...spec
  } = p;
  return spec;
}

/* ------------------------------------------------------------------------ */
/* Validation                                                               */
/* ------------------------------------------------------------------------ */

/** A problem of a field, translated by the editors (cache.issues.<code>). */
export interface CacheIssue {
  /** Dotted field ("ttl.min"). */
  field: string;
  code:
    | 'name'
    | 'duration'
    | 'negative'
    | 'ttlMinDefault'
    | 'ttlDefaultMax'
    | 'ttlMinMax'
    | 'size'
    | 'sizeOrder'
    | 'blockSize'
    | 'header'
    | 'credentialHeader'
    | 'cookie'
    | 'queryParam'
    | 'mediaType'
    | 'algorithm'
    | 'headerValue';
  /** Interpolation values of the message. */
  values?: Record<string, string>;
}

const tokenChars = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

/** An HTTP token (header and cookie names). */
export function isToken(s: string): boolean {
  return s.length > 0 && s.length <= 256 && tokenChars.test(s);
}

/** "type/subtype" with "*" wildcards. */
export function isMediaPattern(s: string): boolean {
  const [t, sub, extra] = s.split('/');
  return (
    extra === undefined &&
    t !== undefined &&
    sub !== undefined &&
    t !== '' &&
    sub !== '' &&
    isToken(t.replaceAll('*', 'x')) &&
    isToken(sub.replaceAll('*', 'x'))
  );
}

/** Lowercase letters, digits, '.', '_' and '-', at most 64, starting with a letter or a digit. */
export function isValidPolicyName(name: string): boolean {
  return /^[a-z0-9][a-z0-9._-]{0,63}$/.test(name);
}

const minBlockSize = 64 * 1024;
const maxBlockSize = 32 * 1024 * 1024;

/**
 * The problems of the set fields of a policy or of the overrides of a
 * level, as internal/service/cache validateSpec reports them.
 */
export function validateSpec(spec: CachePolicySpec): CacheIssue[] {
  const issues: CacheIssue[] = [];
  const durations: Record<string, number | undefined> = {};
  const duration = (field: string, value: CacheDuration | undefined) => {
    if (value === undefined) return;
    const s = parseDuration(value);
    if (s === null) {
      issues.push({ field, code: 'duration', values: { value: String(value) } });
    } else if (s < 0) {
      issues.push({ field, code: 'negative' });
    } else {
      durations[field] = s;
    }
  };
  duration('ttl.default', spec.ttl?.default);
  duration('ttl.min', spec.ttl?.min);
  duration('ttl.max', spec.ttl?.max);
  const [min, def, max] = [
    durations['ttl.min'],
    durations['ttl.default'],
    durations['ttl.max'],
  ];
  if (min !== undefined && def !== undefined && min > def) {
    issues.push({
      field: 'ttl.min',
      code: 'ttlMinDefault',
      values: { min: formatDuration(min), default: formatDuration(def) },
    });
  }
  if (def !== undefined && max !== undefined && def > max) {
    issues.push({
      field: 'ttl.max',
      code: 'ttlDefaultMax',
      values: { default: formatDuration(def), max: formatDuration(max) },
    });
  }
  if (min !== undefined && max !== undefined && min > max && def === undefined) {
    issues.push({
      field: 'ttl.max',
      code: 'ttlMinMax',
      values: { min: formatDuration(min), max: formatDuration(max) },
    });
  }
  const cc = spec.browserCacheControl;
  if (cc !== undefined && (cc.length > 1024 || /[\r\n\0]/.test(cc))) {
    issues.push({ field: 'browserCacheControl', code: 'headerValue' });
  }
  const key = spec.cacheKey;
  if (key?.queryString !== undefined) {
    for (const p of queryStringOf(key.queryString).params) {
      if (/[&=\s]/.test(p)) {
        issues.push({ field: 'cacheKey.queryString', code: 'queryParam', values: { value: p } });
      }
    }
  }
  for (const h of key?.headers ?? []) {
    if (!isToken(h)) {
      issues.push({ field: 'cacheKey.headers', code: 'header', values: { value: h } });
    } else if (['authorization', 'cookie', 'proxy-authorization'].includes(h.toLowerCase())) {
      issues.push({ field: 'cacheKey.headers', code: 'credentialHeader', values: { value: h } });
    }
  }
  for (const c of key?.cookies ?? []) {
    if (!isToken(c)) issues.push({ field: 'cacheKey.cookies', code: 'cookie', values: { value: c } });
  }
  duration('staleWhileRevalidate', spec.staleWhileRevalidate);
  duration('staleIfError', spec.staleIfError);
  duration('negativeCaching', spec.negativeCaching);
  for (const c of spec.bypass?.cookies ?? []) {
    if (c !== '*' && !isToken(c)) {
      issues.push({ field: 'bypass.cookies', code: 'cookie', values: { value: c } });
    }
  }
  for (const h of spec.bypass?.headers ?? []) {
    if (!isToken(h)) issues.push({ field: 'bypass.headers', code: 'header', values: { value: h } });
  }
  for (const q of spec.bypass?.queryParams ?? []) {
    if (q === '' || /[&= ]/.test(q)) {
      issues.push({ field: 'bypass.queryParams', code: 'queryParam', values: { value: q } });
    }
  }
  const seen = new Set<string>();
  for (const a of spec.compression?.algorithms ?? []) {
    if (!['gzip', 'br', 'zstd'].includes(a) || seen.has(a)) {
      issues.push({ field: 'compression.algorithms', code: 'algorithm', values: { value: a } });
    }
    seen.add(a);
  }
  for (const t of spec.compression?.contentTypes ?? []) {
    if (!isMediaPattern(t)) {
      issues.push({ field: 'compression.contentTypes', code: 'mediaType', values: { value: t } });
    }
  }
  const sizes: Record<string, number> = {};
  const size = (field: string, value: CacheSize | undefined) => {
    if (value === undefined) return;
    const b = parseSize(value);
    if (b === null) issues.push({ field, code: 'size', values: { value: String(value) } });
    else sizes[field] = b;
  };
  size('compression.minSize', spec.compression?.minSize);
  size('minSize', spec.minSize);
  size('maxObjectSize', spec.maxObjectSize);
  size('rangeCaching.blockSize', spec.rangeCaching?.blockSize);
  const [lo, hi] = [sizes.minSize, sizes.maxObjectSize];
  if (lo !== undefined && hi !== undefined && lo > hi) {
    issues.push({
      field: 'maxObjectSize',
      code: 'sizeOrder',
      values: { min: formatSize(lo), max: formatSize(hi) },
    });
  }
  const block = sizes['rangeCaching.blockSize'];
  if (block !== undefined && (block < minBlockSize || block > maxBlockSize)) {
    issues.push({ field: 'rangeCaching.blockSize', code: 'blockSize' });
  }
  for (const [field, list] of [
    ['contentTypes.allow', spec.contentTypes?.allow],
    ['contentTypes.deny', spec.contentTypes?.deny],
  ] as const) {
    for (const t of list ?? []) {
      if (!isMediaPattern(t)) issues.push({ field, code: 'mediaType', values: { value: t } });
    }
  }
  return issues;
}

/**
 * The problems of a policy to store: its name, its fields, and the order
 * of its TTL once completed with the standard values (a min above the
 * standard default of 1h needs a default too).
 */
export function validatePolicy(policy: CachePolicy): CacheIssue[] {
  const issues: CacheIssue[] = [];
  if (!isValidPolicyName(policy.name)) issues.push({ field: 'name', code: 'name' });
  const own = validateSpec(specOfPolicy(policy));
  issues.push(...own);
  if (own.length === 0) {
    issues.push(...validateSpec({ ttl: completeSpec(specOfPolicy(policy)).ttl ?? {} }));
  }
  return issues;
}

/** An English sentence of an issue (the mocks answer like the server). */
export function issueText(issue: CacheIssue): string {
  const v = issue.values ?? {};
  switch (issue.code) {
    case 'name':
      return "policy name: lowercase letters, digits, '.', '_' and '-', at most 64";
    case 'duration':
      return `${issue.field}: invalid duration "${v.value ?? ''}"`;
    case 'negative':
      return `${issue.field} must not be negative`;
    case 'ttlMinDefault':
      return `ttl.min ${v.min ?? ''} is above ttl.default ${v.default ?? ''}`;
    case 'ttlDefaultMax':
      return `ttl.default ${v.default ?? ''} is above ttl.max ${v.max ?? ''}`;
    case 'ttlMinMax':
      return `ttl.min ${v.min ?? ''} is above ttl.max ${v.max ?? ''}`;
    case 'size':
      return `${issue.field}: invalid size "${v.value ?? ''}"`;
    case 'sizeOrder':
      return `minSize ${v.min ?? ''} is above maxObjectSize ${v.max ?? ''}`;
    case 'blockSize':
      return 'rangeCaching.blockSize must be between 64KiB and 32MiB';
    case 'credentialHeader':
      return `cacheKey.headers: ${v.value ?? ''} cannot be part of the key`;
    default:
      return `${issue.field}: invalid value "${v.value ?? ''}"`;
  }
}

/* ------------------------------------------------------------------------ */
/* Cache-Control of the browsers                                            */
/* ------------------------------------------------------------------------ */

/** The directives of the Cache-Control sent to the browsers, as the builder edits them. */
export interface CacheControlDirectives {
  visibility: 'public' | 'private' | 'none';
  /** Seconds; undefined: not sent. */
  maxAge?: number | undefined;
  sMaxAge?: number | undefined;
  staleWhileRevalidate?: number | undefined;
  staleIfError?: number | undefined;
  immutable: boolean;
  noCache: boolean;
  noStore: boolean;
  mustRevalidate: boolean;
  /** Directives the builder does not know, kept as written. */
  extra: string[];
}

/** Parses a Cache-Control value into the directives of the builder. */
export function parseCacheControl(value: string): CacheControlDirectives {
  const d: CacheControlDirectives = {
    visibility: 'none',
    immutable: false,
    noCache: false,
    noStore: false,
    mustRevalidate: false,
    extra: [],
  };
  for (const raw of value.split(',')) {
    const token = raw.trim();
    if (token === '') continue;
    const [name = '', arg] = token.split('=', 2);
    const n = name.trim().toLowerCase();
    const seconds = arg !== undefined && /^\d+$/.test(arg.trim()) ? Number(arg.trim()) : undefined;
    switch (n) {
      case 'public':
      case 'private':
        d.visibility = n;
        break;
      case 'max-age':
        if (seconds !== undefined) d.maxAge = seconds;
        else d.extra.push(token);
        break;
      case 's-maxage':
        if (seconds !== undefined) d.sMaxAge = seconds;
        else d.extra.push(token);
        break;
      case 'stale-while-revalidate':
        if (seconds !== undefined) d.staleWhileRevalidate = seconds;
        else d.extra.push(token);
        break;
      case 'stale-if-error':
        if (seconds !== undefined) d.staleIfError = seconds;
        else d.extra.push(token);
        break;
      case 'immutable':
        d.immutable = true;
        break;
      case 'no-cache':
        d.noCache = true;
        break;
      case 'no-store':
        d.noStore = true;
        break;
      case 'must-revalidate':
        d.mustRevalidate = true;
        break;
      default:
        d.extra.push(token);
    }
  }
  return d;
}

/** The Cache-Control value of the directives ("" when none). */
export function formatCacheControl(d: CacheControlDirectives): string {
  const out: string[] = [];
  if (d.visibility !== 'none') out.push(d.visibility);
  if (d.noStore) out.push('no-store');
  if (d.noCache) out.push('no-cache');
  if (d.maxAge !== undefined) out.push(`max-age=${String(d.maxAge)}`);
  if (d.sMaxAge !== undefined) out.push(`s-maxage=${String(d.sMaxAge)}`);
  if (d.staleWhileRevalidate !== undefined) {
    out.push(`stale-while-revalidate=${String(d.staleWhileRevalidate)}`);
  }
  if (d.staleIfError !== undefined) out.push(`stale-if-error=${String(d.staleIfError)}`);
  if (d.mustRevalidate) out.push('must-revalidate');
  if (d.immutable) out.push('immutable');
  out.push(...d.extra);
  return out.join(', ');
}

/** The Cache-Control the clients receive under a policy (internal/service/cache previewOf). */
export function cacheControlOf(spec: CachePolicySpec): { value: string; inherited: boolean } {
  const value = completeSpec(spec).browserCacheControl ?? '';
  return { value, inherited: value === '' };
}

/* ------------------------------------------------------------------------ */
/* Statistics                                                               */
/* ------------------------------------------------------------------------ */

/** The X-Cache values, in the order of the charts (docs/11 §11.7). */
export const cacheResults = ['HIT', 'MISS', 'STALE', 'BYPASS', 'EXPIRED', 'REVALIDATED'] as const;
export type CacheResult = (typeof cacheResults)[number];

/** Requests of a set of counters, every result included. */
export function totalRequests(counters: Pick<CacheCounters, 'requests'>): number {
  return Object.values(counters.requests).reduce((sum, n) => sum + n, 0);
}

/** A hit ratio as a percentage ("93%"); a dash without cacheable request. */
export function formatRatio(ratio: number, requests: number, locale?: string): string {
  if (requests === 0 || !Number.isFinite(ratio)) return '—';
  return new Intl.NumberFormat(locale, { style: 'percent', maximumFractionDigits: 1 }).format(
    ratio,
  );
}

/** Bytes kept in cache by a domain, memory and disk. */
export function hostUsage(h: Pick<CacheHostStats, 'memoryBytes' | 'diskBytes'>): number {
  return h.memoryBytes + h.diskBytes;
}

/* ------------------------------------------------------------------------ */
/* Domains                                                                  */
/* ------------------------------------------------------------------------ */

/** A host matches a domain: equal, or below the wildcard alias "*.<suffix>". */
export function hostMatches(domain: string, host: string): boolean {
  if (domain.startsWith('*.')) return host.endsWith(domain.slice(1));
  return domain === host;
}

/** Whether a host is a domain of a distribution (an alias, or a host a wildcard matches). */
export function isDomainOf(aliases: readonly string[], host: string): boolean {
  return host !== '' && aliases.some((a) => a === host || hostMatches(a, host));
}

/** A host lowercased, without port nor trailing dot (normalizeHost). */
export function normalizeCacheHost(h: string): string {
  let host = h.trim().toLowerCase();
  const i = host.lastIndexOf(':');
  if (i > 0 && !host.slice(i).includes(']') && host.split(':').length === 2) {
    host = host.slice(0, i);
  }
  return host.replace(/\.$/, '');
}
