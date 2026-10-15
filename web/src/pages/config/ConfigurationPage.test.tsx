import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type * as SaveFile from '@/lib/saveFile';
import { saveJsonFile } from '@/lib/saveFile';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { ConfigurationPage } from './ConfigurationPage';

vi.mock('@/lib/saveFile', async (importOriginal) => ({
  ...(await importOriginal<typeof SaveFile>()),
  saveJsonFile: vi.fn(),
  saveTextFile: vi.fn(),
}));

function renderPage() {
  const services = createMockServices({ initialAccessKey: 'nexoadmin' });
  const view = renderRoutes([{ path: '/', element: <ConfigurationPage /> }], { services });
  return { ...view, services };
}

// covers: D-CFG-01
describe('Configuration page (D-CFG-01)', () => {
  // The form re-renders the whole configuration on every keystroke: the
  // values are pasted (one input event) and the test has a generous
  // timeout, so that a loaded machine slows it down without failing it.
  it('edits a subsystem with validation', { timeout: 30_000 }, async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    const region = await screen.findByRole('textbox', { name: 'name' });
    expect(region).toHaveValue('us-east-1');
    await user.clear(region);
    await user.paste('eu-south-1');
    const panel = region.closest('[role="region"]');
    if (!(panel instanceof HTMLElement)) throw new Error('accordion panel not found');
    await user.click(within(panel).getByRole('button', { name: 'Save' }));
    await waitFor(async () => {
      expect((await services.config.exportAll()).region?.name).toBe('eu-south-1');
    });

    await user.click(screen.getByRole('button', { name: /notify_webhook/ }));
    const endpoint = await screen.findByRole('textbox', { name: 'endpoint' });
    await user.click(endpoint);
    await user.paste('not a url');
    expect(await screen.findByText('Enter an http(s) URL.')).toBeInTheDocument();
    const webhook = endpoint.closest('[role="region"]');
    if (!(webhook instanceof HTMLElement)) throw new Error('accordion panel not found');
    expect(within(webhook).getByRole('button', { name: 'Save' })).toBeDisabled();
    // The stored secret comes back masked.
    expect(within(webhook).getByLabelText('auth_token')).toHaveValue('********');
  });

  // Regression (S-OPS-01, D-CFG-01): every key looked effective although
  // only some are applied by the server.
  it('tells which settings the server does not apply yet', async () => {
    const user = userEvent.setup();
    renderPage();
    const region = await screen.findByRole('textbox', { name: 'name' });
    expect(region).toHaveAccessibleDescription(/^Not applied yet/);
    await user.click(screen.getByRole('button', { name: /^api/ }));
    // The label of a switch carries its description.
    const rootAccess = await screen.findByRole('switch', { name: /^root_access/ });
    expect(rootAccess).not.toHaveAccessibleName(/Not applied yet/);
    expect(rootAccess).not.toHaveAccessibleDescription(/Not applied yet/);
    expect(
      await screen.findByRole('textbox', { name: 'cors_allow_origin' }),
    ).toHaveAccessibleDescription(/^Not applied yet/);
    // A subsystem without any applied key is flagged as a whole.
    const scanner = screen.getByRole('button', { name: /^scanner/ });
    expect(scanner).toHaveTextContent('Not applied yet');
    expect(screen.getByRole('button', { name: /^api/ })).not.toHaveTextContent('Not applied yet');
  });

  it('exports and imports the whole configuration', async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    await user.click(await screen.findByRole('button', { name: 'Export' }));
    await waitFor(() => {
      expect(saveJsonFile).toHaveBeenCalledWith(
        'nexo-config.json',
        expect.objectContaining({
          notify_webhook: expect.objectContaining({ auth_token: 'demo-token' }) as unknown,
        }),
      );
    });

    const file = new File([JSON.stringify({ logger: { level: 'debug' } })], 'backup.json', {
      type: 'application/json',
    });
    const input = document.querySelector('input[type="file"]');
    if (!(input instanceof HTMLInputElement)) throw new Error('file input not found');
    await user.upload(input, file);
    const confirm = await screen.findByRole('dialog', { name: 'Import configuration' });
    expect(confirm).toHaveTextContent('Replace the whole configuration with backup.json?');
    await user.click(within(confirm).getByRole('button', { name: 'Import' }));
    await waitFor(async () => {
      expect(await services.config.exportAll()).toEqual({ logger: { level: 'debug' } });
    });
  });

  it('rejects a file that is not a configuration document', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole('button', { name: 'Export' });
    const input = document.querySelector('input[type="file"]');
    if (!(input instanceof HTMLInputElement)) throw new Error('file input not found');
    await user.upload(input, new File(['[1, 2]'], 'list.json', { type: 'application/json' }));
    expect(
      await screen.findByText('list.json is not a configuration document.'),
    ).toBeInTheDocument();
  });
});

// covers: D-ACC-04
describe('Block public access of the cluster (D-ACC-04)', () => {
  it('turns it on for every bucket and changes its mode', async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    const section = await screen.findByRole('region', { name: 'Block public access (cluster)' });
    const toggle = () =>
      within(section).getByRole('switch', { name: 'Block public access on every bucket' });
    // Read-only until the permissions are loaded.
    await waitFor(() => {
      expect(toggle()).toBeEnabled();
    });
    expect(toggle()).not.toBeChecked();
    expect(within(section).getByText('Not blocked')).toBeInTheDocument();

    await user.click(toggle());
    expect(await within(section).findByText('Public access blocked')).toBeInTheDocument();
    expect(toggle()).toBeChecked();
    // It applies to every bucket, whatever its own setting.
    expect(await services.accessRules.getBlockPublic('media')).toMatchObject({
      enabled: false,
      cluster: true,
      effective: true,
    });

    expect(
      within(section).getByText(
        'Allow rules for everyone are refused when saved, the stored ones are ignored.',
      ),
    ).toBeInTheDocument();
    await user.click(within(section).getByRole('radio', { name: 'Ignore' }));
    expect(
      await within(section).findByText('Allow rules for everyone are accepted but ignored.'),
    ).toBeInTheDocument();
    expect(await services.accessRules.getBlockPublic()).toMatchObject({
      enabled: true,
      mode: 'ignore',
    });

    await user.click(toggle());
    expect(await within(section).findByText('Not blocked')).toBeInTheDocument();
    expect(await services.accessRules.getBlockPublic('media')).toMatchObject({
      cluster: false,
      effective: false,
    });
  });
});
