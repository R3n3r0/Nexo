import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { Services } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { distributionRoutes } from '../distributionRoutes';

const routes = distributionRoutes();

function admin() {
  return createMockServices({ initialAccessKey: 'nexoadmin' });
}

function open(path: string, services: Services = admin()) {
  const result = renderRoutes(routes, { initialEntries: [path], services });
  return { ...result, user: userEvent.setup() };
}

/** D-DST-01..07 on the mocks (TD-D-01..05). */
describe('distributions', () => {
  // covers: D-DST-01
  it('D-DST-01 lists the distributions with domains, certificates and traffic', async () => {
    open('/distributions');
    const table = await screen.findByRole('table', { name: 'Distributions' });
    const row = within(table).getByRole('link', { name: 'website' }).closest('tr') as HTMLElement;
    expect(within(row).getByText('www.example.com')).toBeInTheDocument();
    expect(within(row).getByText('*.preview.example.com')).toBeInTheDocument();
    expect(within(row).getByText('media/prod')).toBeInTheDocument();
    // The uploaded certificate expires in 12 days: highlighted.
    expect(
      (await within(row).findByText('Expires in 12 days')).closest('[data-expiry]'),
    ).toHaveAttribute('data-expiry', 'expiring');
    expect(await within(row).findByText('186,550 requests')).toBeInTheDocument();
  });

  // covers: D-DST-01, D-DST-02
  it('TD-D-01 creates a distribution with two domains through the wizard', async () => {
    const { user, services, router } = open('/distributions/-/new');
    const wizard = await screen.findByRole('region', { name: 'New distribution' });
    await user.type(within(wizard).getByRole('textbox', { name: 'Distribution ID' }), 'shop');
    await user.click(within(wizard).getByRole('textbox', { name: 'Bucket' }));
    await user.click(await screen.findByRole('option', { name: 'media' }));
    await user.click(within(wizard).getByRole('button', { name: 'Next' }));

    // Domains: a taken one is reported, then two free ones.
    const domains = within(wizard).getByRole('textbox', { name: 'Domains' });
    await user.type(domains, 'www.example.com{Enter}');
    const states = await within(wizard).findByRole('list', { name: 'Availability of the domains' });
    expect(
      await within(states).findByText('already served by the distribution website'),
    ).toBeInTheDocument();
    await user.click(within(wizard).getByRole('button', { name: 'Next' }));
    expect(
      within(wizard).getByText('A domain is already served: remove it or choose another one.'),
    ).toBeInTheDocument();
    // Remove the taken domain (TagsInput: backspace removes the last tag).
    await user.click(domains);
    await user.keyboard('{Backspace}');
    await user.type(domains, 'shop.example.org{Enter}shop.example.net{Enter}');
    await waitFor(() => {
      const list = within(wizard).getByRole('list', { name: 'Availability of the domains' });
      expect(within(list).getAllByText('Available')).toHaveLength(2);
    });
    const records = within(wizard).getByRole('table', { name: 'DNS records to create' });
    expect(within(records).getAllByText('CNAME')).toHaveLength(2);
    expect(within(records).getByText('shop.example.org')).toBeInTheDocument();
    await user.click(within(wizard).getByRole('button', { name: 'Next' }));
    // Mode, access, TLS: the defaults.
    await user.click(within(wizard).getByRole('button', { name: 'Next' }));
    await user.click(within(wizard).getByRole('button', { name: 'Next' }));
    await user.click(within(wizard).getByRole('button', { name: 'Next' }));
    expect(within(wizard).getByLabelText('Document of the distribution')).toHaveTextContent(
      'aliases: [shop.example.org, shop.example.net]',
    );
    await user.click(within(wizard).getByRole('button', { name: 'Create distribution' }));
    expect(await within(wizard).findByText('Distribution shop created')).toBeInTheDocument();
    expect((await services.distributions.get('shop')).aliases).toEqual([
      'shop.example.org',
      'shop.example.net',
    ]);

    // The routing of both domains is verified.
    await user.click(within(wizard).getByRole('button', { name: 'Verify the routing' }));
    const routing = await within(wizard).findByRole('list', { name: 'Routing of the domains' });
    await waitFor(() => {
      expect(within(routing).getAllByText('Routed')).toHaveLength(2);
    });
    await user.click(within(wizard).getByRole('button', { name: 'Open the distribution' }));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/distributions/shop');
    });
  });

  // covers: D-DST-03, D-DST-04
  it('TD-D-02 adds a behavior, saves with If-Match and tests a URL', async () => {
    const services = admin();
    // Without origin secret: the tester sends no header.
    const sample = await services.distributions.get('website');
    await services.distributions.update({ ...sample, restrictions: {} }, sample.etag);
    const update = vi.spyOn(services.distributions, 'update');
    const { user } = open('/distributions/website/behaviors', services);
    const behaviors = await screen.findByRole('table', { name: 'Behaviors of the distribution' });
    expect(within(behaviors).getByText('/assets/**')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Add behavior' }));
    const dialog = await screen.findByRole('dialog', { name: 'New behavior' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Path pattern' }), '/api/**');
    await user.click(within(dialog).getByRole('textbox', { name: 'Access' }));
    await user.click(await screen.findByRole('option', { name: 'Denied' }));
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }));
    expect(await within(behaviors).findByText('/api/**')).toBeInTheDocument();

    const bar = screen.getByRole('region', { name: 'Unsaved changes' });
    const before = await services.distributions.get('website');
    await user.click(within(bar).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => {
      expect(update).toHaveBeenCalledTimes(1);
    });
    expect(update.mock.calls[0]?.[1]).toBe(before.etag);
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: 'Unsaved changes' })).not.toBeInTheDocument();
    });

    // The URL tester names the behavior serving the URL and highlights it.
    const tester = screen.getByRole('region', { name: 'URL tester' });
    const url = within(tester).getByRole('textbox', { name: 'URL or path' });
    await user.clear(url);
    await user.type(url, 'https://www.example.com/assets/app.js');
    await user.click(within(tester).getByRole('button', { name: 'Test URL' }));
    const result = await within(tester).findByRole('region', { name: 'How the URL is served' });
    expect(within(result).getByText('Behavior 1 (/assets/**)')).toBeInTheDocument();
    expect(within(result).getByText('media/prod/assets/app.js')).toBeInTheDocument();
    expect(within(result).getByText('public, max-age=31536000, immutable')).toBeInTheDocument();
  });

  // covers: D-DST-04
  it('D-DST-04 applies the recommended security headers and the SPA fallback', async () => {
    const { user, services } = open('/distributions/download/responses');
    const headers = await screen.findByRole('region', { name: 'Headers and CORS' });
    await user.click(within(headers).getByRole('button', { name: 'Recommended security headers' }));
    expect(within(headers).getByDisplayValue('nosniff')).toBeInTheDocument();
    await user.click(screen.getByRole('switch', { name: /SPA fallback/ }));
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(async () => {
      const d = await services.distributions.get('download');
      expect(d.headers?.response?.['X-Content-Type-Options']).toBe('nosniff');
      expect(d.defaultBehavior?.spaFallback).toBe('/index.html');
    });
  });

  // covers: D-DST-06
  it('D-DST-06 rotates the origin secret and shows it once', async () => {
    const { user } = open('/distributions/website/security');
    const secret = await screen.findByRole('region', { name: 'Origin secret' });
    await user.click(within(secret).getByRole('button', { name: 'Rotate secret' }));
    const reveal = await screen.findByRole('dialog', { name: 'Origin secret' });
    const value = within(reveal).getByLabelText('X-Origin-Secret');
    expect(value.textContent).toMatch(/^[A-Za-z0-9]{40}$/);
    const done = within(reveal).getByRole('button', { name: 'Done' });
    expect(done).toBeDisabled();
    await user.click(
      within(reveal).getByRole('checkbox', { name: 'I have stored the secret in a safe place' }),
    );
    await user.click(done);
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: 'Origin secret' })).not.toBeInTheDocument();
    });
    expect(within(secret).getByText('2 active secrets in X-Origin-Secret')).toBeInTheDocument();
  });

  // covers: D-DST-06
  it('D-DST-06 edits the client addresses and the rate limit, refusing an invalid address', async () => {
    const { user, services } = open('/distributions/website/security');
    const ip = await screen.findByRole('region', { name: 'Client addresses' });
    const allow = within(ip).getByRole('textbox', { name: 'Allowed (IP or CIDR)' });
    // Read-only until the permissions are loaded.
    await waitFor(() => {
      expect(allow).toBeEnabled();
    });
    await user.type(allow, '10.0.0.0/8{Enter}');
    const deny = within(ip).getByRole('textbox', { name: 'Denied (IP or CIDR)' });
    await user.type(deny, 'not-an-ip{Enter}');
    expect(
      within(ip).getByText('not-an-ip is neither an IP address nor a CIDR.'),
    ).toBeInTheDocument();
    // The server refuses the address: the save is disabled until it is removed.
    const save = screen.getByRole('button', { name: 'Save changes' });
    expect(save).toBeDisabled();
    await user.click(deny);
    await user.keyboard('{Backspace}');
    await waitFor(() => {
      expect(within(ip).queryByText(/neither an IP address nor a CIDR/)).not.toBeInTheDocument();
    });
    expect(save).toBeEnabled();

    const rate = screen.getByRole('region', { name: 'Rate limit' });
    await user.click(within(rate).getByRole('switch', { name: 'Limit the rate' }));
    const rps = within(rate).getByRole('textbox', { name: 'Requests per second' });
    await user.clear(rps);
    await user.type(rps, '50');
    const burst = within(rate).getByRole('textbox', { name: 'Burst' });
    await user.clear(burst);
    await user.type(burst, '80');
    await user.click(save);
    await waitFor(async () => {
      const d = await services.distributions.get('website');
      expect(d.restrictions?.sourceIp).toEqual({ allow: ['10.0.0.0/8'], deny: ['203.0.113.0/24'] });
      expect(d.restrictions?.rateLimit).toMatchObject({ requestsPerSecond: 50, burst: 80 });
    });
  });

  // covers: D-DST-05
  it('TD-D-03 shows the private key of a new key group once and signs a URL with it', async () => {
    const { user, services } = open('/distributions/-/key-groups');
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Create key group' })).toBeEnabled();
    });
    await user.click(screen.getByRole('button', { name: 'Create key group' }));
    const dialog = await screen.findByRole('dialog', { name: 'New key group' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Key group ID' }), 'premium');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    const reveal = await screen.findByRole('dialog', { name: 'Private key' });
    const pem = within(reveal).getByLabelText('Private key (PKCS 8)').textContent ?? '';
    expect(pem).toContain('BEGIN PRIVATE KEY');
    expect(within(reveal).getByRole('button', { name: /Download premium-/ })).toBeInTheDocument();
    await user.click(
      within(reveal).getByRole('checkbox', { name: 'I have stored the secret in a safe place' }),
    );
    await user.click(within(reveal).getByRole('button', { name: 'Done' }));
    const card = await screen.findByRole('region', { name: 'Key group premium' });
    expect(within(card).getByText('1 key')).toBeInTheDocument();
    // The page never shows the private key again.
    expect(screen.queryByText(/BEGIN PRIVATE KEY/)).not.toBeInTheDocument();

    // Signed URL of the signed behavior of a distribution using the group.
    const d = await services.distributions.get('download');
    await services.distributions.update(
      { ...d, defaultBehavior: { access: 'signed', signing: { keyGroup: 'premium' } } },
      d.etag,
    );
    const signer = open('/distributions/download/security', services);
    const section = await screen.findAllByRole('region', { name: 'Signed URLs and cookies' });
    const last = section[section.length - 1] as HTMLElement;
    await signer.user.clear(within(last).getByRole('textbox', { name: 'URL or path' }));
    await signer.user.type(within(last).getByRole('textbox', { name: 'URL or path' }), '/v1.zip');
    await signer.user.click(within(last).getByRole('textbox', { name: 'Private key (PEM)' }));
    await signer.user.paste(pem);
    await signer.user.click(within(last).getByRole('button', { name: 'Sign URL' }));
    const signed = await within(last).findByLabelText('Signed URL');
    expect(signed.textContent).toMatch(
      /^https:\/\/download\.example\.com\/v1\.zip\?Expires=\d+&Signature=.+&Key-Pair-Id=/,
    );
  });

  // covers: D-DST-07
  it('TD-D-04 highlights the expiring certificates and reports an invalid upload', async () => {
    const { user, services } = open('/distributions/-/certificates');
    const table = await screen.findByRole('table', { name: 'TLS certificates' });
    expect(within(table).getByText('Expires in 12 days')).toBeInTheDocument();
    expect(
      screen.getByText('1 certificate expires within 21 days or has expired.'),
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Upload certificate' })).toBeEnabled();
    });
    await user.click(screen.getByRole('button', { name: 'Upload certificate' }));
    const dialog = await screen.findByRole('dialog', { name: 'Upload a certificate' });
    await user.type(
      within(dialog).getByRole('textbox', { name: 'Certificate (PEM)' }),
      'not a certificate',
    );
    await user.type(within(dialog).getByRole('textbox', { name: 'Private key (PEM)' }), 'nope');
    await user.click(within(dialog).getByRole('button', { name: 'Upload certificate' }));
    expect(await within(dialog).findByText('The certificate was refused')).toBeInTheDocument();
    expect(
      within(dialog).getByText('certificatePem holds no PEM certificate.'),
    ).toBeInTheDocument();

    const cert = within(dialog).getByRole('textbox', { name: 'Certificate (PEM)' });
    await user.clear(cert);
    await user.click(cert);
    await user.paste('-----BEGIN CERTIFICATE-----\nCN=cdn.example.com\n-----END CERTIFICATE-----');
    const key = within(dialog).getByRole('textbox', { name: 'Private key (PEM)' });
    await user.clear(key);
    await user.click(key);
    await user.paste('-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----');
    await user.click(within(dialog).getByRole('button', { name: 'Upload certificate' }));
    await waitFor(() => {
      expect(
        screen.queryByRole('dialog', { name: 'Upload a certificate' }),
      ).not.toBeInTheDocument();
    });
    // The ID derived from the domain, as the server does, and the domain.
    expect(await within(table).findAllByText('cdn.example.com')).toHaveLength(2);
    expect((await services.certificates.list()).items).toHaveLength(2);
  });

  // covers: D-DST-01, D-UX-02
  it('TD-D-05 refuses the pages to a user without admin:ManageDistributions', async () => {
    const services = createMockServices({
      accounts: { nexoadmin: 'nexoadmin', reader: 'reader-secret' },
      users: [
        {
          accessKey: 'reader',
          status: 'enabled',
          policyNames: ['readonly'],
          groups: [],
          createdAt: '2026-09-01T00:00:00Z',
          updatedAt: '2026-09-01T00:00:00Z',
        },
      ],
      groups: [],
      initialAccessKey: 'reader',
    });
    open('/distributions/-/new', services);
    expect(
      await screen.findByText('You do not have the permission admin:ManageDistributions.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'New distribution' })).not.toBeInTheDocument();
  });
});
