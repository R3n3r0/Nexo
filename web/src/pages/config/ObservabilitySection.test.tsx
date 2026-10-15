import { configure, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { InfoPage } from '@/pages/InfoPage';
import { createMockServices, sampleServerInfo } from '@/services/mock';
import { renderRoutes, renderWithProviders } from '@/test/render';

import { ConfigurationPage } from './ConfigurationPage';

// The configuration page renders every subsystem: under the load of the
// whole suite its sections take longer to appear than the default waits.
configure({ asyncUtilTimeout: 5_000 });

function renderConfig(services = createMockServices({ initialAccessKey: 'nexoadmin' })) {
  const view = renderRoutes([{ path: '/', element: <ConfigurationPage /> }], { services });
  return { ...view, services, user: userEvent.setup() };
}

// covers: D-CFG-01
describe('Configuration: observability (D-CFG-01)', { timeout: 30_000 }, () => {
  it('sets the level of the logs and goes back to the server default', async () => {
    const { services, user } = renderConfig();
    const section = await screen.findByRole('region', { name: 'Observability' });
    const level = within(section).getByRole('textbox', { name: 'Level' });
    await waitFor(() => {
      expect(level).toBeEnabled();
    });
    await user.click(level);
    await user.click(await screen.findByRole('option', { name: 'debug' }));
    const save = within(section).getAllByRole('button', { name: 'Save' })[0] as HTMLElement;
    await user.click(save);
    await waitFor(async () => {
      expect((await services.config.get('logger')).settings).toEqual({ level: 'debug' });
    });

    await user.click(level);
    await user.click(await screen.findByRole('option', { name: 'Server default' }));
    await user.click(save);
    await waitFor(async () => {
      expect((await services.config.get('logger')).settings).toEqual({});
    });
  });

  it('configures and removes an audit target', async () => {
    const { services, user } = renderConfig();
    const targets = await screen.findByRole('table', { name: 'Audit targets' });
    const webhook = within(targets).getAllByRole('row')[1] as HTMLElement;
    expect(webhook).toHaveTextContent('Not configured');
    await user.click(within(webhook).getByRole('button', { name: 'Edit audit_webhook' }));
    const dialog = await screen.findByRole('dialog', { name: 'Audit target audit_webhook' });
    await user.click(within(dialog).getByRole('switch', { name: /^enable/ }));
    await user.click(within(dialog).getByRole('textbox', { name: 'endpoint' }));
    await user.paste('https://audit.example.com/ingest');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(async () => {
      expect((await services.config.get('audit_webhook')).settings).toEqual({
        enable: 'on',
        endpoint: 'https://audit.example.com/ingest',
      });
    });
    await waitFor(() => {
      expect(
        within(screen.getByRole('table', { name: 'Audit targets' })).getAllByRole('row')[1],
      ).toHaveTextContent('Enabled');
    });
    const row = within(screen.getByRole('table', { name: 'Audit targets' })).getAllByRole(
      'row',
    )[1] as HTMLElement;
    expect(row).toHaveTextContent('https://audit.example.com/ingest');

    await user.click(within(row).getByRole('button', { name: 'Remove audit_webhook' }));
    const confirm = await screen.findByRole('dialog', { name: 'Remove the audit target' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(async () => {
      expect((await services.config.get('audit_webhook')).settings).toEqual({});
    });
  });

  it('configures a named audit target', async () => {
    const { services, user } = renderConfig();
    await screen.findByRole('table', { name: 'Audit targets' });
    await user.click(screen.getByRole('button', { name: 'Named target' }));
    const dialog = await screen.findByRole('dialog', { name: 'Audit target audit_webhook' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Target ID' }), 'bad id');
    expect(
      await within(dialog).findByText('Letters, digits, dot, dash and underscore only.'),
    ).toBeInTheDocument();
    await user.clear(within(dialog).getByRole('textbox', { name: 'Target ID' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Target ID' }), 'siem');
    await screen.findByRole('dialog', { name: 'Audit target audit_webhook:siem' });
    await user.click(within(dialog).getByRole('textbox', { name: 'endpoint' }));
    await user.paste('https://siem.example.com/');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(async () => {
      expect((await services.config.get('audit_webhook:siem')).settings).toEqual({
        endpoint: 'https://siem.example.com/',
      });
    });
  });

  it('shows the tracing and the certificates of the server', async () => {
    renderConfig();
    const tls = await screen.findByRole('table', { name: 'Certificates' });
    expect(tls).toHaveTextContent('nexo.example.com, localhost, 127.0.0.1');
    expect(tls).toHaveTextContent('CN=Example CA');
    expect(screen.getByRole('table', { name: 'TLS' })).toHaveTextContent('TLS 1.2');
    expect(
      screen.getByRole('table', { name: 'Distributed tracing (OpenTelemetry)' }),
    ).toHaveTextContent('Disabled');
  });
});

// covers: D-CFG-02
describe('Info page: TLS and tracing (D-CFG-02)', () => {
  it('flags an expiring certificate and shows the collector', async () => {
    const soon = new Date(Date.now() + 5 * 86_400_000).toISOString();
    const info = sampleServerInfo({
      tls: {
        enabled: true,
        minVersion: '1.3',
        internodeVerify: true,
        certificates: [
          {
            name: 'sni-site',
            domains: ['site.example.org'],
            subject: 'CN=site.example.org',
            issuer: 'CN=R3',
            notBefore: '2026-01-01T00:00:00Z',
            notAfter: soon,
            fingerprint: 'aa'.repeat(32),
          },
          {
            name: 'old',
            domains: ['old.example.org'],
            subject: 'CN=old.example.org',
            issuer: 'CN=R3',
            notBefore: '2020-01-01T00:00:00Z',
            notAfter: '2021-01-01T00:00:00Z',
            fingerprint: 'bb'.repeat(32),
          },
        ],
      },
      telemetry: {
        enabled: true,
        endpoint: 'http://otel-collector:4318',
        protocol: 'http/protobuf',
        sampleRatio: 0.25,
      },
    });
    renderWithProviders(<InfoPage />, {
      services: createMockServices({ info, initialAccessKey: 'nexoadmin' }),
    });
    const certs = await screen.findByRole('table', { name: 'Certificates' });
    const rows = within(certs).getAllByRole('row');
    expect(rows[1]).toHaveTextContent('sni-site');
    expect(rows[1]).toHaveTextContent(/Expires in [45] days/);
    expect(rows[2]).toHaveTextContent('Expired');
    expect(screen.getByRole('table', { name: 'TLS' })).toHaveTextContent('TLS 1.3');
    const tracing = screen.getByRole('table', { name: 'Distributed tracing (OpenTelemetry)' });
    expect(tracing).toHaveTextContent('Exporting spans');
    expect(tracing).toHaveTextContent('http://otel-collector:4318');
    expect(tracing).toHaveTextContent('25%');
  });

  it('tells that plain HTTP is served without certificates', async () => {
    const info = sampleServerInfo({
      tls: { enabled: false, minVersion: '1.2', internodeVerify: false, certificates: [] },
    });
    renderWithProviders(<InfoPage />, {
      services: createMockServices({ info, initialAccessKey: 'nexoadmin' }),
    });
    expect(await screen.findByText('Disabled: the listeners serve plain HTTP')).toBeInTheDocument();
    expect(screen.queryByRole('table', { name: 'Certificates' })).not.toBeInTheDocument();
  });
});
