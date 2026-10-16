import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { api, problemResponse } from '@/test/handlers';
import { server } from '@/test/server';

import { ApiError } from '../errors';
import type { DnsProvider } from '../types';
import { createHttpServices } from './index';

const cf: DnsProvider = {
  name: 'cf',
  type: 'cloudflare',
  cloudflare: {},
  hasCredentials: true,
  usedBy: [],
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
};

/** The DNS providers of the DNS-01 challenge over HTTP (/dns-providers). */
describe('DNS providers over HTTP', () => {
  it('lists, creates, replaces, tests and removes the providers', async () => {
    const seen: string[] = [];
    server.use(
      http.get(api('/dns-providers'), () => HttpResponse.json({ items: [cf] })),
      http.post(api('/dns-providers'), async ({ request }) => {
        seen.push(`POST ${await request.text()}`);
        return HttpResponse.json(cf, { status: 201 });
      }),
      http.put(api('/dns-providers/a%20b'), async ({ request }) => {
        seen.push(`PUT ${await request.text()}`);
        return HttpResponse.json(cf);
      }),
      http.post(api('/dns-providers/cf/test'), async ({ request }) => {
        seen.push(`TEST ${await request.text()}`);
        return HttpResponse.json({
          ok: true,
          record: '_acme-challenge.example.com',
          durationMs: 900,
        });
      }),
      http.delete(api('/dns-providers/cf'), () =>
        problemResponse(409, 'dns_provider_in_use', 'used by site'),
      ),
    );
    const { certificates } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await certificates.listDnsProviders()).toEqual([cf]);
    await certificates.createDnsProvider({
      name: 'cf',
      type: 'cloudflare',
      credentials: { apiToken: 't' },
    });
    await certificates.updateDnsProvider('a b', { type: 'cloudflare', credentials: {} });
    expect(await certificates.testDnsProvider('cf', 'example.com')).toEqual({
      ok: true,
      record: '_acme-challenge.example.com',
      durationMs: 900,
    });
    const err = (await certificates.removeDnsProvider('cf').catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err.code).toBe('dns_provider_in_use');
    expect(seen).toEqual([
      'POST {"name":"cf","type":"cloudflare","credentials":{"apiToken":"t"}}',
      'PUT {"type":"cloudflare","credentials":{}}',
      'TEST {"domain":"example.com"}',
    ]);
  });
});
