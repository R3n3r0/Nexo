import { describe, expect, it } from 'vitest';

import { ApiError } from '../errors';
import { createMockServices } from './index';

const code = async (p: Promise<unknown>) => ((await p.catch((e: unknown) => e)) as ApiError).code;

/** The mock DNS providers follow the rules of the server. */
describe('mock DNS providers', () => {
  it('validates, keeps the credentials, binds the wildcards to dns-01 and protects the providers in use', async () => {
    const { certificates, distributions } = createMockServices({ initialAccessKey: 'nexoadmin' });
    expect((await certificates.listDnsProviders()).map((p) => p.name)).toEqual(['cloudflare-main']);
    expect(await code(certificates.createDnsProvider({ name: 'x', type: 'cloudflare' }))).toBe(
      'invalid_dns_provider',
    );
    expect(
      await code(
        certificates.createDnsProvider({
          name: 'cloudflare-main',
          type: 'cloudflare',
          credentials: { apiToken: 't' },
        }),
      ),
    ).toBe('dns_provider_exists');
    const bind = await certificates.createDnsProvider({
      name: 'bind',
      type: 'rfc2136',
      rfc2136: { nameserver: 'ns1.example.com', tsigKeyName: 'k' },
      credentials: { tsigSecret: 'c2VjcmV0' },
    });
    expect(bind).toMatchObject({ hasCredentials: true, rfc2136: { tsigAlgorithm: 'hmac-sha256' } });
    expect(JSON.stringify(await certificates.listDnsProviders())).not.toContain('c2VjcmV0');
    const kept = await certificates.updateDnsProvider('bind', {
      type: 'rfc2136',
      comment: 'primary',
      rfc2136: { nameserver: 'ns1.example.com', tsigKeyName: 'k' },
    });
    expect(kept.hasCredentials).toBe(true);

    const d = await distributions.get('download');
    expect(
      await code(
        distributions.update({ ...d, aliases: ['*.dl.example.com'], tls: { certificate: 'auto' } }),
      ),
    ).toBe('invalid_distribution');
    const saved = await distributions.update({
      ...d,
      aliases: ['*.dl.example.com'],
      tls: { certificate: 'auto', challenge: 'dns-01', dnsProvider: 'bind' },
    });
    expect(saved.tls).toEqual({ certificate: 'auto', challenge: 'dns-01', dnsProvider: 'bind' });
    const list = await certificates.list();
    expect(list.acme.managed.find((m) => m.distributions.includes('download'))).toMatchObject({
      domains: ['*.dl.example.com'],
      challenge: 'dns-01',
      dnsProvider: 'bind',
    });
    expect(await code(certificates.removeDnsProvider('bind'))).toBe('dns_provider_in_use');
    expect((await certificates.testDnsProvider('bind', '*.example.com')).ok).toBe(true);
    expect(await certificates.testDnsProvider('bind', 'example.invalid')).toMatchObject({
      ok: false,
      step: 'propagation',
    });
  });
});
