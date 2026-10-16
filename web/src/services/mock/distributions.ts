import {
  aliasIssue,
  behaviorFor,
  defaultOriginSecretHeader,
  effectiveDefault,
  inherit,
  isValidDistributionId,
  normalizeAlias,
  normalizePathPrefix,
  pathPrefixIssue,
} from '@/lib/distributions';
import { compileKeyPattern } from '@/lib/keyPattern';

import type { CertificateService, DistributionService, KeyGroupService } from '../interfaces';
import type {
  Certificate,
  CertificateDetails,
  Distribution,
  DistributionBehavior,
  HostTraffic,
  KeyGroup,
  ManagedCertificate,
  PublicKey,
  Route,
} from '../types';
import { clone, problem, type MockContext } from './support';

export interface MockDistributionOptions {
  /** The distributions; samples when the sample buckets are used. */
  distributions?: Distribution[];
  keyGroups?: KeyGroup[];
  certificates?: Certificate[];
  /** Per-alias traffic; null: the server keeps no statistics. */
  traffic?: HostTraffic[] | null;
  /** The S3 and console domains of the deployment (routing kind "main"). */
  mainHosts?: string[];
  /** NEXO_DOMAIN of the virtual hosts (<bucket>.<domain>). */
  domain?: string;
}

/** The state shared by the distribution, key group and certificate mocks. */
export interface MockDistributionState {
  distributions: Map<string, Distribution>;
  versions: Map<string, number>;
  keyGroups: Map<string, KeyGroup>;
  certificates: Map<string, Certificate>;
  traffic: HostTraffic[] | null;
  mainHosts: string[];
  domain: string;
  /** Bucket names, for the virtual hosts. */
  bucketExists: (name: string) => boolean;
  /** Authorizes an action of the signed-in principal (403 otherwise). */
  guard: (action: string) => void;
  /** The caller may read the cache statistics (admin:ManageCache). */
  trafficAllowed: () => boolean;
}

const day = 86_400_000;

/** A PEM block of random-looking base64 (the mocks never do cryptography). */
function fakePem(label: string, seed: string, lines = 4): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
  let x = 0;
  for (const ch of seed) x = (x * 31 + ch.charCodeAt(0)) >>> 0;
  const body: string[] = [];
  for (let l = 0; l < lines; l += 1) {
    let line = '';
    for (let i = 0; i < 64; i += 1) {
      x = (x * 1103515245 + 12345) >>> 0;
      line += alphabet[x % 64];
    }
    body.push(line);
  }
  return `-----BEGIN ${label}-----\n${body.join('\n')}\n-----END ${label}-----\n`;
}

/** The sample distributions, key group and certificates of the demo mode. */
export function sampleDistributions(
  now: Date,
): Required<
  Pick<MockDistributionOptions, 'distributions' | 'keyGroups' | 'certificates' | 'traffic'>
> {
  const iso = (offsetDays: number) => new Date(now.getTime() + offsetDays * day).toISOString();
  const key: PublicKey = {
    id: 'K2JQ5XLM3N7P',
    algorithm: 'ed25519',
    publicKeyPem: fakePem('PUBLIC KEY', 'rilasci', 1),
    createdAt: iso(-30),
  };
  const cert = (
    id: string,
    domains: string[],
    notAfter: number,
    usedBy: string[],
  ): Certificate => ({
    id,
    source: 'upload',
    domains,
    subject: `CN=${domains[0] ?? id}`,
    issuer: 'CN=Example Issuing CA',
    serial: '4f:2a:9c:01',
    notBefore: iso(notAfter - 90),
    notAfter: iso(notAfter),
    fingerprint: '9b2c4e1f7a0d3b6c8e5f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d',
    status: notAfter < 0 ? 'expired' : notAfter < 21 ? 'expiring' : 'valid',
    createdAt: iso(notAfter - 90),
    updatedAt: iso(notAfter - 90),
    usedBy,
  });
  return {
    distributions: [
      {
        id: 'download',
        enabled: true,
        comment: 'Signed downloads of the releases',
        aliases: ['download.example.com'],
        origin: { bucket: 'backups' },
        mode: 'website',
        defaultBehavior: { access: 'signed', signing: { keyGroup: 'rilasci' } },
        tls: { certificate: 'auto' },
        createdAt: iso(-20),
        updatedAt: iso(-20),
      },
      {
        id: 'website',
        enabled: true,
        comment: 'Public website',
        aliases: ['www.example.com', 'example.com', '*.preview.example.com'],
        origin: { bucket: 'media', pathPrefix: '/prod' },
        mode: 'website',
        defaultBehavior: {
          access: 'public',
          allowedMethods: ['GET', 'HEAD', 'OPTIONS'],
          indexDocument: 'index.html',
          spaFallback: '/index.html',
          cache: { cacheControl: 'public, max-age=300' },
        },
        behaviors: [
          {
            pathPattern: { type: 'glob', pattern: '/assets/**' },
            cache: { cacheControl: 'public, max-age=31536000, immutable' },
          },
          {
            pathPattern: { type: 'regex', pattern: '^/downloads/.*\\.(zip|iso)$' },
            access: 'signed',
            signing: { keyGroup: 'rilasci' },
          },
          { pathPattern: { type: 'prefix', pattern: '/internal/' }, access: 'deny' },
        ],
        errorPages: [
          { status: 404, path: '/404.html', responseStatus: 404 },
          { status: 403, path: '/404.html', responseStatus: 404 },
        ],
        redirects: [
          { from: '^/old/(.*)$', to: '/new/$1', status: 301 },
          { hostFrom: 'example.com', toHost: 'www.example.com', status: 301 },
        ],
        headers: {
          response: {
            'Strict-Transport-Security': 'max-age=63072000; includeSubDomains',
            'X-Content-Type-Options': 'nosniff',
          },
        },
        restrictions: {
          sourceIp: { deny: ['203.0.113.0/24'] },
          originSecretHeader: { name: 'X-Origin-Secret', values: ['sha256:5f2c…'] },
        },
        tls: { certificate: 'upload:www-example-com' },
        createdAt: iso(-60),
        updatedAt: iso(-2),
      },
    ],
    keyGroups: [
      {
        id: 'rilasci',
        comment: 'Release downloads',
        keys: [key],
        createdAt: iso(-30),
        updatedAt: iso(-30),
      },
    ],
    certificates: [cert('www-example-com', ['www.example.com', 'example.com'], 12, ['website'])],
    traffic: [
      {
        distribution: 'website',
        host: 'www.example.com',
        requests: 182_340,
        bytes: 9.1e9,
        hitRatio: 0.93,
      },
      {
        distribution: 'website',
        host: 'example.com',
        requests: 4_210,
        bytes: 2.1e7,
        hitRatio: 0.4,
      },
      {
        distribution: 'download',
        host: 'download.example.com',
        requests: 1_204,
        bytes: 6.4e10,
        hitRatio: 0.71,
      },
    ],
  };
}

export function createMockDistributionState(
  ctx: MockContext,
  options: MockDistributionOptions,
  bucketExists: (name: string) => boolean,
  samples: boolean,
  allowed: (action: string) => boolean = () => true,
): MockDistributionState {
  const sample = samples ? sampleDistributions(ctx.now()) : undefined;
  const distributions = new Map<string, Distribution>();
  const versions = new Map<string, number>();
  for (const d of options.distributions ?? sample?.distributions ?? []) {
    distributions.set(d.id, clone({ ...d, etag: etagOf(d.id, 1) }));
    versions.set(d.id, 1);
  }
  return {
    distributions,
    versions,
    keyGroups: new Map((options.keyGroups ?? sample?.keyGroups ?? []).map((k) => [k.id, clone(k)])),
    certificates: new Map(
      (options.certificates ?? sample?.certificates ?? []).map((c) => [c.id, clone(c)]),
    ),
    traffic: options.traffic !== undefined ? clone(options.traffic) : clone(sample?.traffic ?? []),
    mainHosts: options.mainHosts ?? ['localhost', '127.0.0.1'],
    domain: options.domain ?? '',
    bucketExists,
    trafficAllowed: () => allowed('admin:ManageCache'),
    guard(action) {
      if (!allowed(action)) {
        throw problem(403, 'access_denied', `Access denied: ${action}.`);
      }
    },
  };
}

const etagOf = (id: string, version: number) => `${id.length.toString(16)}-${version.toString(16)}`;

function invalid(detail: string) {
  return problem(400, 'invalid_distribution', detail);
}

/** The route of a host in the routing table (exact, wildcard, virtual host, main). */
function resolveHost(state: MockDistributionState, rawHost: string): Route {
  const host = rawHost
    .trim()
    .toLowerCase()
    .replace(/:\d+$/, '')
    .replace(/^\[|\]$/g, '')
    .replace(/\.$/, '');
  const route = (d: Distribution, alias: string): Route => ({
    kind: 'distribution',
    host,
    alias,
    distribution: d.id,
    mode: d.mode ?? 'website',
    enabled: d.enabled !== false,
    bucket: d.origin.bucket,
  });
  for (const d of state.distributions.values()) {
    if (d.aliases.includes(host)) return route(d, host);
  }
  let best: { d: Distribution; alias: string } | undefined;
  for (const d of state.distributions.values()) {
    for (const alias of d.aliases) {
      if (
        alias.startsWith('*.') &&
        host.endsWith(alias.slice(1)) &&
        host.length > alias.length - 1
      ) {
        if (best === undefined || alias.length > best.alias.length) best = { d, alias };
      }
    }
  }
  if (best !== undefined) return route(best.d, best.alias);
  if (state.domain !== '' && host.endsWith(`.${state.domain}`)) {
    const bucket = host.slice(0, -state.domain.length - 1);
    if (state.bucketExists(bucket)) return { kind: 'virtual-host', host, bucket };
  }
  if (state.mainHosts.includes(host)) return { kind: 'main', host };
  return { kind: 'path-style', host };
}

/** Validates and normalizes a distribution like distribution.Validate. */
function validate(state: MockDistributionState, d: Distribution): Distribution {
  if (!isValidDistributionId(d.id)) {
    throw invalid(
      `id "${d.id}": lowercase letters, digits, '.', '_' and '-', at most 64 characters`,
    );
  }
  const mode = d.mode ?? 'website';
  if (d.aliases.length === 0) throw invalid('at least one alias (domain) is required');
  const aliases: string[] = [];
  for (const raw of d.aliases) {
    const alias = normalizeAlias(raw);
    const issue = aliasIssue(alias);
    if (issue === 'wildcard') {
      throw invalid(
        `wildcard alias "${alias}" must be *.<domain> with a domain of at least two labels`,
      );
    }
    if (issue !== undefined) throw invalid(`alias "${alias}" is not a host name`);
    if (aliases.includes(alias)) throw invalid(`alias "${alias}" is listed twice`);
    if (state.mainHosts.includes(alias)) {
      throw problem(
        409,
        'alias_conflict',
        `alias "${alias}" is the S3 or console domain of the deployment`,
      );
    }
    for (const other of state.distributions.values()) {
      if (other.id !== d.id && other.aliases.includes(alias)) {
        throw problem(
          409,
          'alias_conflict',
          `alias "${alias}" is served by the distribution ${other.id}`,
        );
      }
    }
    aliases.push(alias);
  }
  if (d.origin.bucket.trim() === '') throw invalid('origin bucket: the bucket name is required');
  if (pathPrefixIssue(d.origin.pathPrefix ?? '')) {
    throw invalid(
      `origin path prefix "${d.origin.pathPrefix ?? ''}" has an empty, '.' or '..' segment`,
    );
  }
  const prefix = normalizePathPrefix(d.origin.pathPrefix ?? '');
  if (prefix !== '' && mode !== 'website') {
    throw invalid('origin pathPrefix is supported by the website mode only');
  }
  const behaviors = d.behaviors ?? [];
  const check = (b: DistributionBehavior, where: string) => {
    if (b.pathPattern !== undefined) {
      const compiled = compileKeyPattern(b.pathPattern);
      if (!compiled.ok && compiled.issue.unsupported !== true) {
        throw invalid(`${where}: pathPattern: ${compiled.issue.message}`);
      }
    }
    if (b.access === 'signed' && (b.signing?.keyGroup ?? '') === '') {
      throw invalid(`${where}: a signed behavior needs signing.keyGroup`);
    }
    if (b.signing?.keyGroup !== undefined && !state.keyGroups.has(b.signing.keyGroup)) {
      throw problem(
        404,
        'keygroup_not_found',
        `${where}: key group ${b.signing.keyGroup} does not exist`,
      );
    }
  };
  check(d.defaultBehavior ?? {}, 'defaultBehavior');
  behaviors.forEach((b, i) => {
    if (b.pathPattern === undefined)
      throw invalid(`behaviors[${String(i)}]: pathPattern is required`);
    check(b, `behaviors[${String(i)}]`);
  });
  for (const [i, p] of (d.errorPages ?? []).entries()) {
    if (p.status < 400 || p.status > 599) {
      throw invalid(`errorPages[${String(i)}]: status ${String(p.status)} must be 4xx or 5xx`);
    }
  }
  for (const [i, r] of (d.redirects ?? []).entries()) {
    if (r.from !== undefined && r.from !== '') {
      const compiled = compileKeyPattern({ type: 'regex', pattern: r.from });
      if (!compiled.ok && compiled.issue.unsupported !== true) {
        throw invalid(`redirects[${String(i)}]: from: ${compiled.issue.message}`);
      }
    }
  }
  const tls = d.tls?.certificate ?? 'none';
  if (tls.startsWith('upload:') && !state.certificates.has(tls.slice(7))) {
    throw problem(404, 'certificate_not_found', `certificate ${tls.slice(7)} does not exist`);
  }
  const out = clone({
    ...d,
    aliases,
    mode,
    origin: { ...d.origin, ...(prefix !== '' ? { pathPrefix: prefix } : {}) },
  });
  if (prefix === '') delete out.origin.pathPrefix;
  // Origin secrets given in clear are stored hashed.
  const secret = out.restrictions?.originSecretHeader;
  if (secret?.value !== undefined && secret.value !== '') {
    const hashed = secret.value.startsWith('${')
      ? secret.value
      : `sha256:${hashText(secret.value)}`;
    secret.values = [hashed, ...(secret.values ?? [])].slice(0, 8);
    delete secret.value;
  }
  return out;
}

/** A stable hex digest standing for the SHA-256 of a secret. */
function hashText(text: string): string {
  let h = 0x811c9dc5;
  for (const ch of text) h = Math.imul(h ^ ch.charCodeAt(0), 16777619) >>> 0;
  return h.toString(16).padStart(8, '0').repeat(8);
}

function randomToken(length: number): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  const bytes = new Uint8Array(length);
  globalThis.crypto.getRandomValues(bytes);
  return [...bytes].map((b) => alphabet[b % alphabet.length]).join('');
}

/** In-memory DistributionService. */
export function createMockDistributionService(
  ctx: MockContext,
  state: MockDistributionState,
): DistributionService {
  function find(id: string): Distribution {
    const d = state.distributions.get(id);
    if (d === undefined) {
      throw problem(404, 'distribution_not_found', `Distribution ${id} does not exist.`);
    }
    return d;
  }

  function store(d: Distribution, created?: string): Distribution {
    const version = (state.versions.get(d.id) ?? 0) + 1;
    state.versions.set(d.id, version);
    const stored: Distribution = {
      ...d,
      createdAt: created ?? ctx.nowIso(),
      updatedAt: ctx.nowIso(),
      etag: etagOf(d.id, version),
    };
    state.distributions.set(d.id, stored);
    return clone(stored);
  }

  return {
    async list() {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      return [...state.distributions.values()].sort((a, b) => a.id.localeCompare(b.id)).map(clone);
    },
    async get(id) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      return clone(find(id));
    },
    async create(distribution) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      if (state.distributions.has(distribution.id)) {
        throw problem(409, 'conflict', `Distribution ${distribution.id} already exists.`);
      }
      return store(validate(state, distribution));
    },
    async update(distribution, ifMatch) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      const current = find(distribution.id);
      if (ifMatch !== undefined && ifMatch !== '' && ifMatch !== current.etag) {
        throw problem(412, 'precondition_failed', 'The distribution changed since it was read.');
      }
      return store(validate(state, distribution), current.createdAt);
    },
    async remove(id) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      find(id);
      state.distributions.delete(id);
      state.versions.delete(id);
    },
    async test(id, input) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      const d = find(id);
      let url: URL;
      try {
        url = /^https?:\/\//i.test(input.url)
          ? new URL(input.url)
          : new URL(
              input.url.startsWith('/') ? input.url : `/${input.url}`,
              `https://${d.aliases.find((a) => !a.startsWith('*.')) ?? 'localhost'}`,
            );
      } catch {
        throw problem(400, 'invalid_argument', `Invalid URL ${input.url}.`);
      }
      const route = resolveHost(state, url.host);
      if (route.distribution !== d.id) {
        throw problem(
          400,
          'invalid_argument',
          `${url.host} is not an alias of the distribution ${d.id}.`,
        );
      }
      const method = (input.method ?? 'GET').toUpperCase();
      const headers = { ...(d.headers?.response ?? {}) };
      const base = {
        route,
        behaviorIndex: -1,
        behavior: effectiveDefault(d),
        bucket: d.origin.bucket,
        responseHeaders: headers,
      };
      if (d.enabled === false) {
        return { ...base, refusalStatus: 403, refusal: 'the distribution is disabled' };
      }
      if (d.restrictions?.originSecretHeader !== undefined) {
        return { ...base, refusalStatus: 403, refusal: 'the origin secret is missing or wrong' };
      }
      if (d.mode !== 's3') {
        for (const r of d.redirects ?? []) {
          const status = r.status ?? 301;
          if (r.httpsOnly === true && url.protocol === 'http:') {
            return {
              ...base,
              redirectStatus: status,
              redirectLocation: `https://${route.host}${url.pathname}${url.search}`,
            };
          }
          if (r.hostFrom !== undefined && r.hostFrom === route.host) {
            return {
              ...base,
              redirectStatus: status,
              redirectLocation: `${url.protocol}//${r.toHost ?? ''}${url.pathname}${url.search}`,
            };
          }
          if (r.from !== undefined && r.from !== '') {
            try {
              const re = new RegExp(r.from.replace(/\(\?P</g, '(?<'));
              const path = decodeURIComponent(url.pathname);
              if (re.test(path)) {
                return {
                  ...base,
                  redirectStatus: status,
                  redirectLocation: path.replace(re, (r.to ?? '').replace(/\$\{(\w+)\}/g, '$<$1>')),
                };
              }
            } catch {
              // Not a JavaScript expression: the server decides.
            }
          }
        }
      }
      const path = decodeURIComponent(url.pathname);
      const index = behaviorFor(d, path);
      const behaviors = d.behaviors ?? [];
      const behavior = index < 0 ? effectiveDefault(d) : inherit(d, behaviors[index] ?? {});
      const result = {
        ...base,
        behaviorIndex: index,
        behavior,
        responseHeaders: { ...headers, ...(behavior.responseHeaders ?? {}) },
      };
      if (!(behavior.allowedMethods ?? []).includes(method)) {
        return { ...result, refusalStatus: 405, refusal: `method ${method} is not allowed` };
      }
      const rel = path.slice(1);
      if (rel.split('/').some((seg) => seg === '.' || seg === '..') || rel.includes('\\')) {
        return { ...result, refusalStatus: 404, refusal: 'invalid path' };
      }
      if (behavior.access === 'deny') {
        return { ...result, refusalStatus: 403, refusal: 'access denied by the behavior' };
      }
      const prefix = (d.origin.pathPrefix ?? '').replace(/^\/+|\/+$/g, '');
      return { ...result, key: `${prefix === '' ? '' : `${prefix}/`}${rel}` };
    },
    async rotateOriginSecret(id, input = {}) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      const d = clone(find(id));
      const secret = randomToken(40);
      const current = d.restrictions?.originSecretHeader;
      const header = input.header ?? current?.name ?? defaultOriginSecretHeader;
      const keep = input.dropPrevious === true ? 0 : (input.keep ?? 1);
      const previous = header === current?.name ? (current.values ?? []).slice(0, keep) : [];
      d.restrictions = {
        ...d.restrictions,
        originSecretHeader: { name: header, values: [`sha256:${hashText(secret)}`, ...previous] },
      };
      const stored = store(d, d.createdAt);
      return { secret, header, distribution: stored };
    },
    async sign(id, input) {
      await ctx.delay();
      state.guard('admin:SignDistributionURL');
      const d = find(id);
      if (!input.privateKeyPem.includes('PRIVATE KEY')) {
        throw problem(400, 'invalid_argument', 'privateKeyPem is not a PEM private key.');
      }
      const has = (input.expiresIn ?? 0) > 0;
      if (has === (input.expiresAt !== undefined && input.expiresAt !== '')) {
        throw problem(400, 'invalid_argument', 'Exactly one of expiresIn and expiresAt.');
      }
      const host = input.host ?? d.aliases.find((a) => !a.startsWith('*.')) ?? '';
      const url = /^https?:\/\//i.test(input.url)
        ? new URL(input.url)
        : new URL(
            input.url.startsWith('/') ? input.url : `/${input.url}`,
            `${input.scheme ?? 'https'}://${host}`,
          );
      const index = behaviorFor(d, decodeURIComponent(url.pathname));
      const behavior = index < 0 ? effectiveDefault(d) : inherit(d, d.behaviors?.[index] ?? {});
      if (behavior.access !== 'signed') {
        throw problem(
          400,
          'invalid_argument',
          `${url.pathname} is not served by a signed behavior.`,
        );
      }
      const group = state.keyGroups.get(behavior.signing?.keyGroup ?? '');
      const keyPairId = input.keyPairId ?? group?.keys[0]?.id ?? '';
      if (group === undefined || !group.keys.some((k) => k.id === keyPairId)) {
        throw problem(404, 'key_not_found', 'The key is not in the key group of the behavior.');
      }
      const expiresAt = has
        ? new Date(ctx.now().getTime() + (input.expiresIn ?? 0) * 1000).toISOString()
        : (input.expiresAt ?? '');
      const expires = String(Math.floor(Date.parse(expiresAt) / 1000));
      const signature = randomToken(86);
      const custom =
        (input.resource ?? '') !== '' ||
        (input.ipRange ?? '') !== '' ||
        (input.notBefore ?? '') !== '';
      if (input.cookies === true) {
        const cookies = custom
          ? [
              { name: 'CloudFront-Policy', value: randomToken(120), expires: expiresAt },
              { name: 'CloudFront-Signature', value: signature, expires: expiresAt },
              { name: 'CloudFront-Key-Pair-Id', value: keyPairId, expires: expiresAt },
            ]
          : [
              { name: 'CloudFront-Expires', value: expires, expires: expiresAt },
              { name: 'CloudFront-Signature', value: signature, expires: expiresAt },
              { name: 'CloudFront-Key-Pair-Id', value: keyPairId, expires: expiresAt },
            ];
        return { url: url.toString(), cookies, keyGroup: group.id, keyPairId, expiresAt };
      }
      const signed = new URL(url.toString());
      if (custom) signed.searchParams.set('Policy', randomToken(120));
      else signed.searchParams.set('Expires', expires);
      signed.searchParams.set('Signature', signature);
      signed.searchParams.set('Key-Pair-Id', keyPairId);
      return { url: signed.toString(), cookies: [], keyGroup: group.id, keyPairId, expiresAt };
    },
    async resolve(host) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      return resolveHost(state, host);
    },
    async traffic(distribution) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      if (!state.trafficAllowed()) return null;
      if (state.traffic === null) return null;
      return {
        hosts: clone(
          state.traffic.filter(
            (h) =>
              distribution === undefined || distribution === '' || h.distribution === distribution,
          ),
        ),
      };
    },
  };
}

/** In-memory KeyGroupService (keys generated as fake PEM blocks). */
export function createMockKeyGroupService(
  ctx: MockContext,
  state: MockDistributionState,
): KeyGroupService {
  function find(id: string): KeyGroup {
    const g = state.keyGroups.get(id);
    if (g === undefined)
      throw problem(404, 'keygroup_not_found', `Key group ${id} does not exist.`);
    return g;
  }

  function generate(algorithm: 'ed25519' | 'rsa', bits: number | undefined, comment?: string) {
    const id = randomToken(14).toUpperCase();
    const publicKey: PublicKey = {
      id,
      algorithm,
      ...(algorithm === 'rsa' ? { bits: bits ?? 2048 } : {}),
      publicKeyPem: fakePem('PUBLIC KEY', id, algorithm === 'rsa' ? 6 : 1),
      ...(comment !== undefined && comment !== '' ? { comment } : {}),
      createdAt: ctx.nowIso(),
    };
    return {
      publicKey,
      privateKeyPem: fakePem('PRIVATE KEY', `${id}-private`, algorithm === 'rsa' ? 25 : 1),
    };
  }

  return {
    async list() {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      return [...state.keyGroups.values()].sort((a, b) => a.id.localeCompare(b.id)).map(clone);
    },
    async get(id) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      return clone(find(id));
    },
    async create(input) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      if (!isValidDistributionId(input.id)) {
        throw problem(400, 'invalid_argument', `Key group id ${input.id} is not valid.`);
      }
      if (state.keyGroups.has(input.id)) {
        throw problem(409, 'conflict', `Key group ${input.id} already exists.`);
      }
      const group: KeyGroup = {
        id: input.id,
        ...(input.comment !== undefined && input.comment !== '' ? { comment: input.comment } : {}),
        keys: [],
        createdAt: ctx.nowIso(),
        updatedAt: ctx.nowIso(),
      };
      let key;
      if (input.generateKey !== undefined) {
        key = generate(
          input.generateKey.algorithm ?? 'ed25519',
          input.generateKey.bits,
          input.generateKey.comment,
        );
        group.keys.push(key.publicKey);
      }
      state.keyGroups.set(group.id, group);
      return { keyGroup: clone(group), ...(key !== undefined ? { key: clone(key) } : {}) };
    },
    async remove(id) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      find(id);
      for (const d of state.distributions.values()) {
        for (const b of [d.defaultBehavior, ...(d.behaviors ?? [])]) {
          if (b?.signing?.keyGroup === id) {
            throw problem(409, 'conflict', `Key group ${id} is used by the distribution ${d.id}.`);
          }
        }
      }
      state.keyGroups.delete(id);
    },
    async keys(id) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      return clone(find(id).keys);
    },
    async addKey(id, input) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      const group = find(id);
      if (input.publicKeyPem !== undefined && input.publicKeyPem !== '') {
        if (!input.publicKeyPem.includes('BEGIN PUBLIC KEY')) {
          throw problem(400, 'invalid_argument', 'publicKeyPem is not a PKIX public key.');
        }
        const publicKey: PublicKey = {
          id: randomToken(14).toUpperCase(),
          algorithm: input.algorithm ?? 'ed25519',
          publicKeyPem: input.publicKeyPem,
          ...(input.comment !== undefined && input.comment !== ''
            ? { comment: input.comment }
            : {}),
          createdAt: ctx.nowIso(),
        };
        group.keys.push(publicKey);
        group.updatedAt = ctx.nowIso();
        return { publicKey: clone(publicKey) };
      }
      const key = generate(input.algorithm ?? 'ed25519', input.bits, input.comment);
      group.keys.push(key.publicKey);
      group.updatedAt = ctx.nowIso();
      return clone(key);
    },
    async removeKey(id, keyId) {
      await ctx.delay();
      state.guard('admin:ManageDistributions');
      const group = find(id);
      if (!group.keys.some((k) => k.id === keyId)) {
        throw problem(404, 'key_not_found', `Key ${keyId} is not in the key group ${id}.`);
      }
      group.keys = group.keys.filter((k) => k.id !== keyId);
      group.updatedAt = ctx.nowIso();
    },
  };
}

/** Days the mock certificates uploaded with a PEM are valid. */
const uploadedValidityDays = 90;

/** In-memory CertificateService: a PEM "expiring" in its text expires in 10 days. */
export function createMockCertificateService(
  ctx: MockContext,
  state: MockDistributionState,
): CertificateService {
  function usedBy(id: string): string[] {
    return [...state.distributions.values()]
      .filter((d) => d.tls?.certificate === `upload:${id}`)
      .map((d) => d.id);
  }
  function withUse(c: Certificate): Certificate {
    return { ...clone(c), usedBy: usedBy(c.id) };
  }
  return {
    async list() {
      await ctx.delay();
      state.guard('admin:ManageCertificates');
      const managed: ManagedCertificate[] = [...state.distributions.values()]
        .filter((d) => d.tls?.certificate === 'auto')
        .map((d) => ({
          id: d.aliases[0] ?? d.id,
          domains: d.aliases.filter((a) => !a.startsWith('*.')),
          distributions: [d.id],
          state: 'pending',
        }));
      return {
        items: [...state.certificates.values()]
          .sort((a, b) => a.id.localeCompare(b.id))
          .map(withUse),
        acme: {
          directory: 'https://acme-v02.api.letsencrypt.org/directory',
          challenge: 'http-01',
          managed,
        },
      };
    },
    async get(id) {
      await ctx.delay();
      state.guard('admin:ManageCertificates');
      const c = state.certificates.get(id);
      if (c === undefined)
        throw problem(404, 'certificate_not_found', `Certificate ${id} does not exist.`);
      return withUse(c);
    },
    async upload(input) {
      await ctx.delay();
      state.guard('admin:ManageCertificates');
      if (!input.certificatePem.includes('BEGIN CERTIFICATE')) {
        throw problem(400, 'invalid_certificate', 'certificatePem holds no PEM certificate.');
      }
      if (!/BEGIN (?:RSA |EC )?PRIVATE KEY/.test(input.privateKeyPem)) {
        throw problem(400, 'invalid_certificate', 'privateKeyPem holds no PEM private key.');
      }
      const domain = /CN=([^\s,]+)/.exec(input.certificatePem)?.[1] ?? 'example.com';
      // Like the server: the first DNS name without "*.", lowercase, other characters as '-'.
      const derived = domain
        .toLowerCase()
        .replace(/^\*\./, '')
        .replace(/[^a-z0-9.-]/g, '-')
        .replace(/^[-._]+|[-._]+$/g, '');
      const id = input.id !== undefined && input.id !== '' ? input.id : derived || 'cert';
      const now = ctx.now();
      const days = input.certificatePem.includes('expiring') ? 10 : uploadedValidityDays;
      const details: CertificateDetails = {
        id,
        source: 'upload',
        domains: [domain],
        subject: `CN=${domain}`,
        issuer: `CN=${domain}`,
        serial: '01',
        notBefore: now.toISOString(),
        notAfter: new Date(now.getTime() + days * day).toISOString(),
        fingerprint: hashText(input.certificatePem),
        status: days < 21 ? 'expiring' : 'valid',
        createdAt: now.toISOString(),
        updatedAt: now.toISOString(),
      };
      const cert: Certificate = { ...details, usedBy: [] };
      state.certificates.set(id, cert);
      return withUse(cert);
    },
    async remove(id) {
      await ctx.delay();
      state.guard('admin:ManageCertificates');
      if (!state.certificates.has(id)) {
        throw problem(404, 'certificate_not_found', `Certificate ${id} does not exist.`);
      }
      const users = usedBy(id);
      if (users.length > 0) {
        throw problem(409, 'conflict', `Certificate ${id} is used by ${users.join(', ')}.`);
      }
      state.certificates.delete(id);
    },
  };
}
