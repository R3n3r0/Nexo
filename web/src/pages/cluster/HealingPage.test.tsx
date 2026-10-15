import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createMockServices, sampleClusterInfo } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { HealingPage } from './HealingPage';

// covers: D-CLU-02
describe('HealingPage (D-CLU-02)', () => {
  it('explains that a single drive has nothing to heal', async () => {
    renderWithProviders(<HealingPage />, {
      services: createMockServices({ initialAccessKey: 'nexoadmin' }),
    });
    expect(await screen.findByRole('status')).toHaveTextContent(
      'Healing needs an erasure-coded deployment',
    );
    expect(screen.getByRole('button', { name: 'Start healing' })).toBeDisabled();
  });

  it('starts a job with its options and shows its results drive by drive', async () => {
    const user = userEvent.setup();
    renderWithProviders(<HealingPage />, {
      services: createMockServices({ info: sampleClusterInfo(), initialAccessKey: 'nexoadmin' }),
    });
    expect(await screen.findByText('No healing job in the last hour.')).toBeInTheDocument();

    await user.click(screen.getByRole('textbox', { name: 'Bucket' }));
    await user.click(await screen.findByRole('option', { name: 'backups' }));
    await user.type(screen.getByRole('textbox', { name: 'Prefix or object' }), '2026/');
    await user.click(screen.getByRole('checkbox', { name: 'Deep: verify the bitrot checksums' }));
    await user.click(screen.getByRole('button', { name: 'Start healing' }));

    const details = await screen.findByRole('region', { name: 'Job heal-0001' });
    expect(details).toHaveTextContent('backups/2026/');
    // The job finishes on a later poll, with the copy rebuilt on the healing drive.
    await waitFor(
      () => {
        expect(within(details).getByText('Done')).toBeInTheDocument();
      },
      { timeout: 4000 },
    );
    const drives = within(details).getByRole('table', { name: 'Results by drive' });
    expect(
      within(drives)
        .getAllByRole('row')
        .filter((r) => r.dataset.state === 'offline'),
    ).toHaveLength(2);
    const items = within(details).getByRole('table', { name: 'Items that needed healing' });
    expect(items).toHaveTextContent('backups/2026/report.pdf');
    expect(items).toHaveTextContent('disk 6: missing → ok');
    expect(items).toHaveTextContent('disk 3: offline → offline');
    expect(items).toHaveTextContent('1 copy rebuilt');

    const jobs = screen.getByRole('table', { name: 'Healing jobs' });
    expect(jobs).toHaveTextContent('heal-0001');
    expect(jobs).toHaveTextContent('recursive');
    expect(jobs).toHaveTextContent('deep');
  });

  it('shows the background healing of the nodes and the drives being healed', async () => {
    renderWithProviders(<HealingPage />, {
      services: createMockServices({ info: sampleClusterInfo(), initialAccessKey: 'nexoadmin' }),
    });
    const background = await screen.findByRole('region', { name: 'Background healing' });
    const queues = within(background).getByRole('table', {
      name: 'Background healing queue of the nodes',
    });
    expect(within(queues).getAllByRole('row')).toHaveLength(4);
    expect(queues).toHaveTextContent('nexo1:9000');
    expect(background).toHaveTextContent('/data2 on nexo3:9000');
    expect(background).toHaveTextContent('128 objects healed (3 GiB), 0 failed, 1 buckets done');
  });
});
