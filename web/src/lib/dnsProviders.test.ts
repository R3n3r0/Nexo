import { describe, expect, it } from 'vitest';

import type { DnsProvider } from '@/services';

import { wildcardNeedingDns } from './distributions';
import { dnsProviderForm, dnsProviderInput, dnsProviderTarget } from './dnsProviders';

const provider = (patch: Partial<DnsProvider>): DnsProvider => ({
  name: 'p',
  type: 'cloudflare',
  hasCredentials: true,
  usedBy: [],
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
  ...patch,
});

describe('DNS providers', () => {
  it('sends only the settings and the credentials of the type, never empty secrets', () => {
    const form = {
      ...dnsProviderForm(undefined),
      name: ' bind ',
      type: 'rfc2136' as const,
      apiToken: 'left over from cloudflare',
      nameserver: 'ns1.example.com',
      tsigKeyName: 'nexo-acme',
      tsigSecret: '',
      propagationTimeout: 180 as const,
    };
    expect(dnsProviderInput(form)).toEqual({
      name: 'bind',
      type: 'rfc2136',
      propagationTimeout: 180,
      rfc2136: { nameserver: 'ns1.example.com', tsigKeyName: 'nexo-acme' },
      credentials: {},
    });
    expect(
      dnsProviderInput({ ...dnsProviderForm(undefined), name: 'cf', apiToken: 'tok' }),
    ).toEqual({ name: 'cf', type: 'cloudflare', cloudflare: {}, credentials: { apiToken: 'tok' } });
    expect(
      dnsProviderInput({
        ...dnsProviderForm(undefined),
        name: 'aws',
        type: 'route53',
        region: 'eu-west-1',
      }),
    ).toEqual({ name: 'aws', type: 'route53', route53: { region: 'eu-west-1' }, credentials: {} });
  });

  it('edits a provider from its settings, the secrets empty', () => {
    const form = dnsProviderForm(
      provider({ type: 'route53', route53: { accessKeyId: 'AKID', hostedZoneId: 'Z1' } }),
    );
    expect(form).toMatchObject({ type: 'route53', accessKeyId: 'AKID', hostedZoneId: 'Z1' });
    expect(form.secretAccessKey).toBe('');
  });

  it('names the zone or the server of a provider', () => {
    expect(dnsProviderTarget(provider({ cloudflare: { zoneId: 'z1' } }), 'ambient')).toBe('z1');
    expect(dnsProviderTarget(provider({ type: 'route53', route53: {} }), 'ambient')).toBe(
      '— (ambient)',
    );
    expect(
      dnsProviderTarget(
        provider({ type: 'rfc2136', rfc2136: { nameserver: 'ns1', tsigKeyName: 'k' } }),
        'ambient',
      ),
    ).toBe('ns1 (k)');
  });

  it('flags a wildcard alias without the DNS-01 challenge', () => {
    const d = {
      id: 'p',
      enabled: true,
      aliases: ['p.example.com', '*.p.example.com'],
      origin: { bucket: 'b' },
      tls: { certificate: 'auto' },
    };
    expect(wildcardNeedingDns(d)).toBe('*.p.example.com');
    expect(
      wildcardNeedingDns({
        ...d,
        tls: { certificate: 'auto', challenge: 'dns-01', dnsProvider: 'cf' },
      }),
    ).toBeUndefined();
    expect(wildcardNeedingDns({ ...d, tls: { certificate: 'upload:x' } })).toBeUndefined();
  });
});
