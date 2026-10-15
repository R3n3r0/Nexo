import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { saveJsonFile } from '@/lib/saveFile';
import type { CreatedAccessKey } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { AccessKeysPage } from './AccessKeysPage';

vi.mock('@/lib/saveFile', () => ({ saveJsonFile: vi.fn(), saveTextFile: vi.fn() }));

function renderPage(options: Parameters<typeof createMockServices>[0] = {}, path = '/access-keys') {
  const services = createMockServices({ initialAccessKey: 'nexoadmin', ...options });
  const view = renderRoutes([{ path: '/access-keys', element: <AccessKeysPage /> }], {
    initialEntries: [path],
    services,
  });
  return { ...view, services };
}

// covers: D-IAM-04
describe('Access keys (D-IAM-04)', () => {
  it('shows the secret of a new key once, with copy and credentials download', async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    const create = vi.spyOn(services.accessKeys, 'create');
    await user.click(await screen.findByRole('button', { name: 'Create access key' }));
    const form = await screen.findByRole('form', { name: 'Create access key' });
    await user.type(within(form).getByRole('textbox', { name: 'Name' }), 'laptop');
    await user.click(within(form).getByRole('button', { name: 'Create' }));

    const dialog = await screen.findByRole('dialog', { name: 'Access key created' });
    const created = (await create.mock.results[0]?.value) as CreatedAccessKey | undefined;
    if (!created) throw new Error('create was not called');
    const secret = created.secretKey;
    expect(within(dialog).getByText(secret)).toBeInTheDocument();
    expect(within(dialog).getByRole('alert')).toHaveTextContent(/only time/);

    await user.click(within(dialog).getByRole('button', { name: 'Copy Secret key' }));
    await waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe(secret);
    });
    await user.click(within(dialog).getByRole('button', { name: 'Download credentials (JSON)' }));
    expect(saveJsonFile).toHaveBeenCalledWith(
      `${created.accessKey}-credentials.json`,
      expect.objectContaining({ accessKey: created.accessKey, secretKey: secret, api: 's3v4' }),
    );

    // Closing requires an explicit acknowledgement; afterwards the secret is gone.
    const done = within(dialog).getByRole('button', { name: 'Done' });
    expect(done).toBeDisabled();
    await user.click(within(dialog).getByRole('checkbox', { name: 'I have saved the secret key' }));
    await user.click(done);
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: 'Access key created' })).not.toBeInTheDocument();
    });
    expect(await screen.findByText(created.accessKey)).toBeInTheDocument();
    expect(screen.queryByText(secret)).not.toBeInTheDocument();
    // The list never carries secrets.
    const keys = await services.accessKeys.list();
    expect(JSON.stringify(keys)).not.toContain(secret);
  });

  it('creates a key with expiration and an inline policy', async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    const create = vi.spyOn(services.accessKeys, 'create');
    await user.click(await screen.findByRole('button', { name: 'Create access key' }));
    const form = await screen.findByRole('form', { name: 'Create access key' });
    await user.click(within(form).getByRole('switch', { name: /Restrict with an inline policy/ }));
    expect(await within(form).findByRole('textbox', { name: 'Inline policy' })).toBeInTheDocument();
    await user.click(within(form).getByRole('switch', { name: 'Expires' }));
    await user.click(within(form).getByRole('button', { name: 'Create' }));
    // The expiration is required once enabled.
    expect(await within(form).findByText('Pick the expiration date.')).toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
    await user.click(within(form).getByRole('switch', { name: 'Expires' }));
    await user.click(within(form).getByRole('button', { name: 'Create' }));
    await screen.findByRole('dialog', { name: 'Access key created' });
    expect(create).toHaveBeenCalledWith(
      expect.objectContaining({
        policy: expect.objectContaining({ Version: '2012-10-17' }) as unknown,
      }),
    );
  });

  it('edits the status and deletes a key', async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    await user.click(await screen.findByRole('button', { name: 'Edit AKIA0MOCKDEPLOY0001' }));
    const form = await screen.findByRole('form');
    await user.click(within(form).getByRole('switch', { name: 'Enabled' }));
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    await waitFor(async () => {
      expect((await services.accessKeys.get('AKIA0MOCKDEPLOY0001')).status).toBe('disabled');
    });

    await user.click(screen.getByRole('button', { name: 'Delete AKIA0MOCKDEPLOY0001' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete access key' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(() => {
      expect(screen.queryByText('AKIA0MOCKDEPLOY0001')).not.toBeInTheDocument();
    });
  });

  it('shows a user only its own keys, without the owner filter', async () => {
    renderPage({
      accounts: { nexoadmin: 'nexoadmin', alice: 'alice-secret' },
      initialAccessKey: 'alice',
    });
    expect(await screen.findByText('No access keys.')).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: 'Owner' })).not.toBeInTheDocument();
  });

  it('filters the keys of a user from the URL for administrators', async () => {
    renderPage({}, '/access-keys?user=alice');
    expect(await screen.findByText('No access keys.')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Owner' })).toHaveValue('alice');
  });
});
