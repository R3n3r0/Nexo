import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createMockServices, sampleClusterInfo } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { ClusterMaintenance } from './ClusterMaintenance';

// covers: S-OPS-09, C-ADM-09
describe('ClusterMaintenance (S-OPS-09)', () => {
  it('starts a rolling update after checking the URL and the SHA-256, and follows it', async () => {
    const user = userEvent.setup();
    const services = createMockServices({
      info: sampleClusterInfo(),
      initialAccessKey: 'nexoadmin',
    });
    renderWithProviders(<ClusterMaintenance />, { services });
    const section = await screen.findByRole('region', { name: 'Maintenance' });
    expect(
      await within(section).findByText('No update or coordinated restart has run yet.'),
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(within(section).getByRole('button', { name: 'Update all nodes' })).toBeEnabled();
    });
    await user.click(within(section).getByRole('button', { name: 'Update all nodes' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Update the binary of every node',
    });
    await user.type(within(dialog).getByRole('textbox', { name: /^Binary URL/ }), 'ftp://x');
    await user.type(within(dialog).getByRole('textbox', { name: /^SHA-256/ }), 'abc');
    await user.click(within(dialog).getByRole('button', { name: 'Start the update' }));
    expect(
      within(dialog).getByText(
        'Enter the http(s) URL of the binary for the platform of the nodes.',
      ),
    ).toBeInTheDocument();
    expect(within(dialog).getByText('64 hexadecimal digits.')).toBeInTheDocument();
    expect(await services.maintenance.job()).toBeNull();

    const url = within(dialog).getByRole('textbox', { name: /^Binary URL/ });
    await user.clear(url);
    await user.type(url, 'https://example.com/nexo');
    const sha = within(dialog).getByRole('textbox', { name: /^SHA-256/ });
    await user.clear(sha);
    await user.type(sha, 'a'.repeat(64));
    await user.click(within(dialog).getByRole('button', { name: 'Start the update' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    const nodes = await within(section).findByRole('table', { name: 'Nodes of the job' });
    expect(within(nodes).getAllByRole('row')).toHaveLength(5);
    expect(section).toHaveTextContent('to v1.1.0');
    expect(section).toHaveTextContent('Rolling');
    // The buttons wait for the running job.
    expect(within(section).getByRole('button', { name: 'Restart cluster' })).toBeDisabled();
  });

  it('stops the cluster only once the word is typed', async () => {
    const user = userEvent.setup();
    const services = createMockServices({
      info: sampleClusterInfo(),
      initialAccessKey: 'nexoadmin',
    });
    renderWithProviders(<ClusterMaintenance />, { services });
    const section = await screen.findByRole('region', { name: 'Maintenance' });
    await waitFor(() => {
      expect(within(section).getByRole('button', { name: 'Stop cluster' })).toBeEnabled();
    });
    await user.click(within(section).getByRole('button', { name: 'Stop cluster' }));
    const dialog = await screen.findByRole('dialog', { name: 'Stop every node' });
    const submit = within(dialog).getByRole('button', { name: 'Stop the cluster' });
    expect(submit).toBeDisabled();
    await user.type(
      within(dialog).getByRole('textbox', { name: 'Type "stop" to confirm' }),
      'stop',
    );
    await user.click(submit);
    expect(await within(section).findByText(/Stop requested/)).toBeInTheDocument();
  });

  it('starts a coordinated restart all at once', async () => {
    const user = userEvent.setup();
    const services = createMockServices({
      info: sampleClusterInfo(),
      initialAccessKey: 'nexoadmin',
    });
    renderWithProviders(<ClusterMaintenance />, { services });
    const section = await screen.findByRole('region', { name: 'Maintenance' });
    await waitFor(() => {
      expect(within(section).getByRole('button', { name: 'Restart cluster' })).toBeEnabled();
    });
    await user.click(within(section).getByRole('button', { name: 'Restart cluster' }));
    const dialog = await screen.findByRole('dialog', { name: 'Restart every node' });
    await user.click(within(dialog).getByText('All at once'));
    await user.click(within(dialog).getByRole('button', { name: 'Restart' }));
    expect(await within(section).findByText('Coordinated restart')).toBeInTheDocument();
    const job = await services.maintenance.job();
    expect(job).toMatchObject({ kind: 'restart', mode: 'all' });
  });
});
