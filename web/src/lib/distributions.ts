/**
 * Helpers of the distributions pages (D-DST-01..08, docs/10 §10.2): the
 * validation of the aliases and of the ids like the server, the DNS
 * records to create, the inherited fields of the behaviors, the
 * recommended security headers and the certificates in expiry.
 */
import type {
  CertificateDetails,
  Distribution,
  DistributionAccess,
  DistributionBehavior,
  HostTraffic,
  KeyPattern,
  Route,
} from '@/services';

import { compileKeyPattern } from './keyPattern';

/** Methods a behavior may allow (POST and PUT only on signed website behaviors). */
export const behaviorMethods = ['GET', 'HEAD', 'OPTIONS', 'POST', 'PUT'] as const;
export const defaultMethods = ['GET', 'HEAD', 'OPTIONS'];
export const accessModes: readonly DistributionAccess[] = [
  'public',
  'signed',
  'authenticated',
  'deny',
];

/** Days before the expiry from which a certificate is highlighted (docs/10: 21 days). */
export const expiryWarningDays = 21;

/** The header of the origin secret when the distribution names none. */
export const defaultOriginSecretHeader = 'X-Origin-Secret';

/** Lowercase letters, digits, then also '.', '_' and '-' (at most 64). */
export function isValidDistributionId(id: string): boolean {
  return (
    id.length > 0 &&
    id.length <= 64 &&
    id !== '.' &&
    id !== '..' &&
    /^[a-z0-9][a-z0-9._-]*$/.test(id)
  );
}

function validHostname(h: string): boolean {
  if (h === '' || h.length > 253 || isIpLiteral(h)) return false;
  return h
    .split('.')
    .every(
      (label) =>
        label !== '' && label.length <= 63 && /^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?$/.test(label),
    );
}

export function isIpLiteral(h: string): boolean {
  return /^\d{1,3}(?:\.\d{1,3}){3}$/.test(h) || (h.includes(':') && /^[0-9a-fA-F:.]+$/.test(h));
}

/** The alias as the server stores it (lowercase, no trailing dot). */
export function normalizeAlias(alias: string): string {
  return alias.trim().toLowerCase().replace(/\.$/, '');
}

/**
 * Validates an alias like the server: an exact host name or "*." followed
 * by a host name of at least two labels.
 */
export function aliasIssue(alias: string): 'invalid' | 'wildcard' | undefined {
  const a = normalizeAlias(alias);
  if (a.startsWith('*.')) {
    const rest = a.slice(2);
    return validHostname(rest) && rest.includes('.') ? undefined : 'wildcard';
  }
  return !a.includes('*') && validHostname(a) ? undefined : 'invalid';
}

export function isWildcardAlias(alias: string): boolean {
  return alias.startsWith('*.');
}

/** A DNS record to create for an alias. */
export interface DnsRecord {
  name: string;
  type: 'CNAME' | 'A' | 'AAAA';
  value: string;
}

/**
 * The records that point an alias to the cluster (or to the proxy in front
 * of it): a CNAME to a host name, A/AAAA to addresses. The apex of a zone
 * (two labels, e.g. example.com) cannot have a CNAME: A/AAAA records to
 * the addresses of the target are needed there.
 */
export function dnsRecords(alias: string, target: string): DnsRecord[] {
  const name = normalizeAlias(alias);
  const value = target.trim().replace(/\.$/, '');
  if (value === '') return [];
  if (isIpLiteral(value)) {
    return [{ name, type: value.includes(':') ? 'AAAA' : 'A', value }];
  }
  if (isApex(name)) {
    return [
      { name, type: 'A', value: `<IPv4 of ${value}>` },
      { name, type: 'AAAA', value: `<IPv6 of ${value}>` },
    ];
  }
  return [{ name, type: 'CNAME', value: `${value}.` }];
}

/** A name of two labels (example.com): the apex of its zone. */
export function isApex(name: string): boolean {
  return !name.startsWith('*.') && name.split('.').length === 2;
}

/** The path prefix as the server normalizes it ("/a/b", "" for the whole bucket). */
export function normalizePathPrefix(prefix: string): string {
  const p = prefix.trim().replace(/^\/+|\/+$/g, '');
  return p === '' ? '' : `/${p}`;
}

/** True when a path prefix has an empty, "." or ".." segment (refused). */
export function pathPrefixIssue(prefix: string): boolean {
  const p = prefix.trim().replace(/^\/+|\/+$/g, '');
  if (p === '') return false;
  return p.split('/').some((seg) => seg === '' || seg === '.' || seg === '..') || p.includes('\\');
}

/** The default behavior with its defaults filled in. */
export function effectiveDefault(d: Distribution): DistributionBehavior {
  const b = d.defaultBehavior ?? {};
  return {
    ...b,
    access: b.access ?? 'public',
    allowedMethods: b.allowedMethods ?? defaultMethods,
  };
}

/** A behavior with the unset fields inherited from the default behavior. */
export function inherit(d: Distribution, behavior: DistributionBehavior): DistributionBehavior {
  const base = effectiveDefault(d);
  const out: DistributionBehavior = { ...base, ...stripUndefined(behavior) };
  if (behavior.pathPattern !== undefined) out.pathPattern = behavior.pathPattern;
  else delete out.pathPattern;
  return out;
}

function stripUndefined<T extends object>(value: T): Partial<T> {
  return Object.fromEntries(Object.entries(value).filter(([, v]) => v !== undefined)) as Partial<T>;
}

/**
 * The behavior serving a path: the first behavior whose pattern matches,
 * -1 for the default one. A pattern that does not compile never matches.
 */
export function behaviorFor(d: Distribution, path: string): number {
  const behaviors = d.behaviors ?? [];
  for (const [index, behavior] of behaviors.entries()) {
    const pattern = behavior.pathPattern;
    if (pattern === undefined) continue;
    const compiled = compileKeyPattern(pattern);
    if (compiled.ok && compiled.compiled.match(path).matched) return index;
  }
  return -1;
}

/** A short text of a pattern for the lists. */
export function patternLabel(pattern: KeyPattern | undefined): string {
  return pattern === undefined ? '' : pattern.pattern;
}

/** Recommended response headers (D-DST-04 preset). */
export const securityHeadersPreset: Readonly<Record<string, string>> = {
  'Strict-Transport-Security': 'max-age=63072000; includeSubDomains',
  'X-Content-Type-Options': 'nosniff',
  'X-Frame-Options': 'SAMEORIGIN',
  'Referrer-Policy': 'strict-origin-when-cross-origin',
  'Permissions-Policy': 'camera=(), microphone=(), geolocation=()',
};

/** Days left before a date, a started day counting as one (0 or less once passed). */
export function daysLeft(notAfter: string, now: Date = new Date()): number {
  return Math.ceil((Date.parse(notAfter) - now.getTime()) / 86_400_000);
}

/** The state of a certificate in the console: expired, expiring (< 21 days), ok. */
export function expiryState(
  cert: Pick<CertificateDetails, 'notAfter' | 'status'>,
  now: Date = new Date(),
): 'expired' | 'expiring' | 'ok' {
  const left = Date.parse(cert.notAfter) - now.getTime();
  if (cert.status === 'expired' || left <= 0) return 'expired';
  if (cert.status === 'expiring' || left < expiryWarningDays * 86_400_000) return 'expiring';
  return 'ok';
}

/** The TLS mode of a distribution: none, auto (ACME) or an uploaded certificate. */
export function tlsMode(d: Distribution): { mode: 'none' | 'auto' | 'upload'; id?: string } {
  const c = d.tls?.certificate ?? 'none';
  if (c === 'auto') return { mode: 'auto' };
  if (c.startsWith('upload:')) return { mode: 'upload', id: c.slice('upload:'.length) };
  return { mode: 'none' };
}

/** The key groups the signed behaviors of a distribution use. */
export function signingKeyGroups(d: Distribution): string[] {
  const groups = new Set<string>();
  for (const b of [d.defaultBehavior, ...(d.behaviors ?? [])]) {
    if (b?.access === 'signed' && b.signing?.keyGroup !== undefined) groups.add(b.signing.keyGroup);
  }
  return [...groups];
}

/** A minimal new distribution of the wizard. */
export function newDistribution(): Distribution {
  return {
    id: '',
    enabled: true,
    aliases: [],
    origin: { bucket: '' },
    mode: 'website',
    defaultBehavior: { access: 'public', indexDocument: 'index.html' },
    tls: { certificate: 'none' },
  };
}

/** Removes the server-managed fields before a PUT. */
export function editable(d: Distribution): Distribution {
  const { createdAt: _c, updatedAt: _u, etag: _e, ...rest } = d;
  return rest;
}

/** What an alias resolves to now, for the distribution being edited. */
export type AliasState = 'invalid' | 'available' | 'mine' | 'taken' | 'main' | 'checking';

export function aliasState(
  alias: string,
  route: Route | undefined,
  distribution?: string,
): AliasState {
  if (aliasIssue(alias) !== undefined) return 'invalid';
  if (route === undefined) return 'checking';
  if (route.kind === 'main') return 'main';
  if (route.kind === 'distribution') {
    if (route.distribution === distribution) return 'mine';
    // A wildcard alias of another distribution does not take an exact alias.
    return route.alias === normalizeAlias(alias) ? 'taken' : 'available';
  }
  return 'available';
}

/** Requests and bytes summed over the aliases of a distribution. */
export function trafficTotals(hosts: readonly HostTraffic[]): { requests: number; bytes: number } {
  return hosts.reduce(
    (acc, h) => ({ requests: acc.requests + h.requests, bytes: acc.bytes + h.bytes }),
    { requests: 0, bytes: 0 },
  );
}
