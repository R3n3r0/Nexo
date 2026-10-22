import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { RemoteTiers } from './RemoteTiers';

// covers: S-STO-12
describe('RemoteTiers (S-STO-12)', () => {
  it('adds a tier, lists it without its secret and removes it', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    renderWithProviders(<RemoteTiers />, { services });
    const section = await screen.findByRole('region', { name: 'Remote tiers' });
    expect(
      await within(section).findByText('No remote tier: the lifecycle transitions need one.'),
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(within(section).getByRole('button', { name: 'Add a tier' })).toBeEnabled();
    });
    await user.click(within(section).getByRole('button', { name: 'Add a tier' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a remote tier' });
    // Invalid fields are refused before anything is sent.
    await user.type(within(dialog).getByRole('textbox', { name: /^S3 endpoint/ }), 'not a url');
    await user.click(within(dialog).getByRole('button', { name: 'Add' }));
    expect(within(dialog).getByText('Enter an http(s)://host[:port] URL.')).toBeInTheDocument();
    expect(await services.tiers.list()).toEqual([]);

    await user.type(within(dialog).getByRole('textbox', { name: /^Name/ }), 'cold');
    const endpoint = within(dialog).getByRole('textbox', { name: /^S3 endpoint/ });
    await user.clear(endpoint);
    await user.type(endpoint, 'https://archive.example.com/');
    await user.type(within(dialog).getByRole('textbox', { name: /^Bucket/ }), 'cold-data');
    await user.type(within(dialog).getByRole('textbox', { name: /^Prefix/ }), 'nexo/');
    await user.type(within(dialog).getByRole('textbox', { name: /^Access key/ }), 'ARCHKEY');
    await user.type(within(dialog).getByLabelText(/^Secret key/), 'ARCHSECRET');
    await user.click(within(dialog).getByRole('button', { name: 'Add' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    const table = await within(section).findByRole('table', { name: 'Remote tiers' });
    expect(within(table).getByText('COLD')).toBeInTheDocument();
    expect(within(table).getByText('https://archive.example.com')).toBeInTheDocument();
    expect(within(table).getByText('cold-data/nexo')).toBeInTheDocument();
    expect(within(table).queryByText('ARCHSECRET')).not.toBeInTheDocument();
    const [stored] = await services.tiers.list();
    expect(stored).toMatchObject({
      name: 'COLD',
      bucket: 'cold-data',
      prefix: 'nexo/',
      accessKey: 'ARCHKEY',
    });
    expect(stored).not.toHaveProperty('secretKey');

    await user.click(within(table).getByRole('button', { name: 'Remove tier COLD' }));
    const confirm = await screen.findByRole('dialog', { name: 'Remove the tier' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    expect(
      await within(section).findByText('No remote tier: the lifecycle transitions need one.'),
    ).toBeInTheDocument();
  });
});
