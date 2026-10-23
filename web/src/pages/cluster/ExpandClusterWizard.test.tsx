import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createMockServices, sampleClusterInfo } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { PoolManagement } from './PoolManagement';

// covers: S-STO-08, D-CLU-03
describe('ExpandClusterWizard (S-STO-08)', () => {
  it('checks a new pool, gives the new command line and stops the cluster after the checklist', async () => {
    const user = userEvent.setup();
    const services = createMockServices({
      info: sampleClusterInfo(),
      initialAccessKey: 'nexoadmin',
    });
    renderWithProviders(<PoolManagement />, { services });
    const section = await screen.findByRole('region', { name: 'Pool management' });
    await user.click(await within(section).findByRole('button', { name: 'Expand cluster' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Change the pools of the deployment',
    });

    const input = within(dialog).getByRole('textbox', { name: 'New pool 2' });
    // A pool that cannot be split into erasure sets is refused by the check.
    await user.type(input, 'http://nexo{{5...5}:9000/data{{1...1}');
    await user.click(within(dialog).getByRole('button', { name: 'Check the topology' }));
    expect(await within(dialog).findByText(/cannot split the drives/)).toBeInTheDocument();

    await user.clear(input);
    await user.type(input, 'http://nexo{{5...8}:9000/data{{1...2}');
    await user.click(within(dialog).getByRole('button', { name: 'Check the topology' }));
    expect(
      await within(dialog).findByText(
        'Valid topology: 2 pools on 8 nodes, as every node will parse it.',
      ),
    ).toBeInTheDocument();
    const table = within(dialog).getByRole('table', { name: 'Planned pools' });
    expect(table).toHaveTextContent('8 drives: 1 × 8 (EC:4)');
    expect(within(table).getByText('New')).toBeInTheDocument();
    expect(dialog).toHaveTextContent('New nodes: nexo5:9000, nexo6:9000, nexo7:9000, nexo8:9000');
    expect(dialog).toHaveTextContent(
      'NEXO_ENDPOINTS="http://nexo{1...4}:9000/data{1...2} http://nexo{5...8}:9000/data{1...2}"',
    );

    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    const stop = within(dialog).getByRole('button', { name: 'Stop the cluster now' });
    expect(stop).toBeDisabled();
    for (const box of within(dialog).getAllByRole('checkbox')) {
      await user.click(box);
    }
    await waitFor(() => {
      expect(stop).toBeEnabled();
    });
    await user.click(stop);
    expect(
      await within(dialog).findByText(/Stop requested\. Start every node/),
    ).toBeInTheDocument();
  });

  it('plans the removal of a decommissioned pool', async () => {
    const user = userEvent.setup();
    const info = sampleClusterInfo();
    const services = createMockServices({
      info,
      initialAccessKey: 'nexoadmin',
      pools: {
        pools: [
          {
            index: 1,
            id: 'http://nexo{1...4}:9000/data{1...2}',
            total: 1,
            free: 1,
            draining: false,
          },
          {
            index: 2,
            id: 'http://nexo{5...8}:9000/data{1...2}',
            total: 1,
            free: 1,
            draining: true,
            decommission: {
              state: 'complete',
              startedAt: '2026-10-07T08:00:00Z',
              pass: 1,
              objects: 3,
              versions: 3,
              bytes: 10,
              failed: 0,
              pendingUploads: 0,
            },
          },
        ],
      },
    });
    renderWithProviders(<PoolManagement />, { services });
    const section = await screen.findByRole('region', { name: 'Pool management' });
    await user.click(await within(section).findByRole('button', { name: 'Plan the removal' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Change the pools of the deployment',
    });
    await user.click(
      within(dialog).getByRole('checkbox', {
        name: 'Pool 2 (http://nexo{5...8}:9000/data{1...2})',
      }),
    );
    await user.click(within(dialog).getByRole('button', { name: 'Check the topology' }));
    const table = await within(dialog).findByRole('table', { name: 'Planned pools' });
    expect(within(table).getByText('Removed')).toBeInTheDocument();
    expect(dialog).toHaveTextContent('NEXO_ENDPOINTS="http://nexo{1...4}:9000/data{1...2}"');
  });
});
