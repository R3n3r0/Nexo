import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { SiteReplication } from './SiteReplication';

// covers: S-STO-11
describe('SiteReplication (S-STO-11)', () => {
  it('groups sites, shows their synchronization and removes the group', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    renderWithProviders(<SiteReplication />, { services });
    const section = await screen.findByRole('region', { name: 'Site replication' });
    expect(
      await within(section).findByText('This deployment is in no site replication group.'),
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(within(section).getByRole('button', { name: 'Group sites' })).toBeEnabled();
    });
    await user.click(within(section).getByRole('button', { name: 'Group sites' }));
    const dialog = await screen.findByRole('dialog', { name: 'Group sites' });
    const fill = async (group: string, name: string, endpoint: string, admin: string) => {
      const box = within(within(dialog).getByRole('group', { name: group }));
      await user.type(box.getByRole('textbox', { name: /^Name/ }), name);
      await user.type(box.getByRole('textbox', { name: /^S3 endpoint/ }), endpoint);
      await user.type(box.getByRole('textbox', { name: /^Admin API endpoint/ }), admin);
    };
    // Invalid URLs are refused before anything is sent.
    await fill('This site', 'site-a', 'not a url', 'https://console.a.example.com');
    await user.click(within(dialog).getByRole('button', { name: 'Group' }));
    expect(
      within(dialog).getAllByText('Enter an http(s)://host[:port] URL.').length,
    ).toBeGreaterThan(0);
    const endpoint = within(within(dialog).getByRole('group', { name: 'This site' })).getByRole(
      'textbox',
      {
        name: /^S3 endpoint/,
      },
    );
    await user.clear(endpoint);
    await user.type(endpoint, 'https://s3.a.example.com');
    await fill('Site 1', 'site-b', 'https://s3.b.example.com/', 'https://console.b.example.com');
    await user.click(within(dialog).getByRole('button', { name: 'Group' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    const table = await within(section).findByRole('table', { name: 'Site replication' });
    expect(within(table).getByText('site-a')).toBeInTheDocument();
    expect(within(table).getByText('https://s3.b.example.com')).toBeInTheDocument();
    expect(await within(section).findByText('In sync')).toBeInTheDocument();
    expect((await services.siteReplication.info()).sites.map((s) => s.name)).toEqual([
      'site-a',
      'site-b',
    ]);

    await user.click(within(section).getByRole('button', { name: 'Synchronize now' }));
    await user.click(within(section).getByRole('button', { name: 'Remove the group' }));
    const confirm = await screen.findByRole('dialog', { name: 'Remove the site replication' });
    await user.click(within(confirm).getByRole('button', { name: 'Remove the group' }));
    expect(
      await within(section).findByText('This deployment is in no site replication group.'),
    ).toBeInTheDocument();
  });
});
