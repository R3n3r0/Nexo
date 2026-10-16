import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { Services } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { distributionRoutes } from '../distributionRoutes';

const routes = distributionRoutes();

function open(
  path: string,
  services: Services = createMockServices({ initialAccessKey: 'nexoadmin' }),
) {
  const result = renderRoutes(routes, { initialEntries: [path], services });
  return { ...result, user: userEvent.setup() };
}

/** D-DST-07: the DNS providers of the DNS-01 challenge and the wildcard certificates. */
describe('DNS providers', () => {
  // covers: D-DST-07
  it('adds an RFC 2136 provider, tests it and keeps its secret out of the page', async () => {
    const { user, services } = open('/distributions/-/certificates');
    const section = await screen.findByRole(
      'region',
      { name: 'DNS providers' },
      { timeout: 10_000 },
    );
    const table = await within(section).findByRole(
      'table',
      { name: 'DNS providers' },
      { timeout: 10_000 },
    );
    expect(within(table).getByText('cloudflare-main')).toBeInTheDocument();

    await waitFor(() => {
      expect(within(section).getByRole('button', { name: 'Add DNS provider' })).toBeEnabled();
    });
    await user.click(within(section).getByRole('button', { name: 'Add DNS provider' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a DNS provider' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Name' }), 'bind');
    await user.click(within(dialog).getByRole('textbox', { name: 'Type' }));
    await user.click(await screen.findByRole('option', { name: 'RFC 2136 (TSIG)' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Nameserver' }), 'ns1.example.com');
    await user.type(within(dialog).getByRole('textbox', { name: 'TSIG key name' }), 'nexo-acme');
    await user.type(within(dialog).getByLabelText(/TSIG secret/), 'c2VjcmV0LXRzaWc=');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: 'Add a DNS provider' })).not.toBeInTheDocument();
    });
    expect(await within(table).findByText('ns1.example.com (nexo-acme)')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('c2VjcmV0LXRzaWc=');
    const bind = (await services.certificates.listDnsProviders()).find((p) => p.name === 'bind');
    expect(bind?.hasCredentials).toBe(true);

    await user.click(within(table).getByRole('button', { name: 'Test the DNS provider bind' }));
    const test = await screen.findByRole('dialog', { name: 'Test the DNS provider bind' });
    await user.type(within(test).getByRole('textbox', { name: 'Domain' }), 'example.invalid');
    await user.click(within(test).getByRole('button', { name: 'Run the test' }));
    expect(await within(test).findByText(/Failed at the propagation step/)).toBeInTheDocument();
    const domain = within(test).getByRole('textbox', { name: 'Domain' });
    await user.clear(domain);
    await user.type(domain, 'example.com');
    await user.click(within(test).getByRole('button', { name: 'Run the test' }));
    expect(
      await within(test).findByText(
        /_acme-challenge.example.com published, served by the nameservers/,
      ),
    ).toBeInTheDocument();
  }, 60_000);

  // covers: D-DST-07
  it('requires the DNS-01 challenge of a wildcard domain and saves it with a provider', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const d = await services.distributions.get('download');
    await services.distributions.update(
      { ...d, aliases: ['download.example.com', '*.dl.example.com'], tls: { certificate: 'none' } },
      d.etag,
    );
    const { user } = open('/distributions/download/tls', services);
    const panel = await screen.findByRole('region', { name: 'TLS' }, { timeout: 10_000 });
    await waitFor(() => {
      expect(within(panel).getByRole('radio', { name: /ACME/ })).toBeEnabled();
    });
    await user.click(within(panel).getByRole('radio', { name: /ACME/ }));
    expect(
      await within(panel).findByText(
        'The wildcard domain *.dl.example.com needs the DNS-01 challenge with a DNS provider.',
      ),
    ).toBeInTheDocument();
    await user.click(within(panel).getByRole('textbox', { name: 'ACME challenge' }));
    await user.click(await screen.findByRole('option', { name: /DNS-01/ }));
    await waitFor(() => {
      expect(within(panel).queryByText(/needs the DNS-01 challenge/)).not.toBeInTheDocument();
    });
    expect(within(panel).getByRole('textbox', { name: 'DNS provider' })).toHaveValue(
      'cloudflare-main (Cloudflare)',
    );
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(async () => {
      expect((await services.distributions.get('download')).tls).toEqual({
        certificate: 'auto',
        challenge: 'dns-01',
        dnsProvider: 'cloudflare-main',
      });
    });
  }, 60_000);
});
