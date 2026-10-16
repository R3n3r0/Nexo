/**
 * The DNS providers of the ACME DNS-01 challenge in the console: the form
 * of a provider and its request (the settings and credentials of its type
 * only; the empty credentials keep the stored ones).
 */
import type { DnsProvider, DnsProviderInput, DnsProviderType } from '@/services';

/** The zone or the server of a provider, for the table. */
export function dnsProviderTarget(p: DnsProvider, ambient: string): string {
  if (p.cloudflare !== undefined) return p.cloudflare.zoneId ?? '—';
  if (p.route53 !== undefined) {
    const zone = p.route53.hostedZoneId ?? '—';
    return p.route53.accessKeyId === undefined || p.route53.accessKeyId === ''
      ? `${zone} (${ambient})`
      : zone;
  }
  if (p.rfc2136 !== undefined) {
    return p.rfc2136.tsigKeyName !== undefined && p.rfc2136.tsigKeyName !== ''
      ? `${p.rfc2136.nameserver} (${p.rfc2136.tsigKeyName})`
      : p.rfc2136.nameserver;
  }
  return '—';
}

/** The form of a provider: the settings of every type, the secrets write-only. */
export interface DnsProviderForm {
  name: string;
  type: DnsProviderType;
  comment: string;
  zoneId: string;
  apiToken: string;
  accessKeyId: string;
  secretAccessKey: string;
  sessionToken: string;
  region: string;
  hostedZoneId: string;
  nameserver: string;
  zone: string;
  tsigKeyName: string;
  tsigAlgorithm: string;
  tsigSecret: string;
  propagationTimeout: number | '';
}

export function dnsProviderForm(p: DnsProvider | undefined): DnsProviderForm {
  return {
    name: p?.name ?? '',
    type: p?.type ?? 'cloudflare',
    comment: p?.comment ?? '',
    zoneId: p?.cloudflare?.zoneId ?? '',
    apiToken: '',
    accessKeyId: p?.route53?.accessKeyId ?? '',
    secretAccessKey: '',
    sessionToken: '',
    region: p?.route53?.region ?? '',
    hostedZoneId: p?.route53?.hostedZoneId ?? '',
    nameserver: p?.rfc2136?.nameserver ?? '',
    zone: p?.rfc2136?.zone ?? '',
    tsigKeyName: p?.rfc2136?.tsigKeyName ?? '',
    tsigAlgorithm: p?.rfc2136?.tsigAlgorithm ?? '',
    tsigSecret: '',
    propagationTimeout: p?.propagationTimeout ?? '',
  };
}

/** The request of a form: the settings and the credentials of its type only. */
export function dnsProviderInput(f: DnsProviderForm): DnsProviderInput {
  const opt = (v: string) => (v.trim() !== '' ? v.trim() : undefined);
  const strip = <T extends object>(o: T): T =>
    Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined)) as T;
  const base: DnsProviderInput = strip({
    name: f.name.trim(),
    type: f.type,
    comment: opt(f.comment),
    propagationTimeout: f.propagationTimeout === '' ? undefined : f.propagationTimeout,
  });
  switch (f.type) {
    case 'cloudflare':
      return {
        ...base,
        cloudflare: strip({ zoneId: opt(f.zoneId) }),
        credentials: strip({ apiToken: opt(f.apiToken) }),
      };
    case 'route53':
      return {
        ...base,
        route53: strip({
          accessKeyId: opt(f.accessKeyId),
          region: opt(f.region),
          hostedZoneId: opt(f.hostedZoneId),
        }),
        credentials: strip({
          secretAccessKey: opt(f.secretAccessKey),
          sessionToken: opt(f.sessionToken),
        }),
      };
    case 'rfc2136':
      return {
        ...base,
        rfc2136: strip({
          nameserver: f.nameserver.trim(),
          zone: opt(f.zone),
          tsigKeyName: opt(f.tsigKeyName),
          tsigAlgorithm: opt(f.tsigAlgorithm) as
            NonNullable<DnsProviderInput['rfc2136']>['tsigAlgorithm'] | undefined,
        }),
        credentials: strip({ tsigSecret: opt(f.tsigSecret) }),
      };
  }
}
