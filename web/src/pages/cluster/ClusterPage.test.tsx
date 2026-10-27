import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createMockServices, sampleClusterInfo } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { ClusterPage } from './ClusterPage';

// covers: D-CLU-01
describe('ClusterPage (D-CLU-01)', () => {
  it('highlights the offline node and its drives, the degraded set and the drifting clock', async () => {
    renderWithProviders(<ClusterPage />, {
      services: createMockServices({ info: sampleClusterInfo(), initialAccessKey: 'nexoadmin' }),
    });
    const alerts = await screen.findAllByRole('alert');
    expect(alerts).toHaveLength(2);
    expect(alerts[0]).toHaveTextContent('1 node offline');
    expect(alerts[0]).toHaveTextContent('nexo2:9000');
    expect(alerts[1]).toHaveTextContent('Clock skew between the nodes');
    expect(alerts[1]).toHaveTextContent('The clock of nexo4:9000 is off by more than 2 s');

    const nodes = screen.getByRole('table', { name: 'Nodes' });
    const rows = within(nodes).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(4);
    expect(rows[0]).toHaveTextContent('This node');
    expect(rows[0]).toHaveTextContent('Reference');
    expect(rows[0]).toHaveTextContent('2 pending, 14 healed');
    expect(rows[1]).toHaveAttribute('data-state', 'offline');
    expect(rows[1]).toHaveTextContent('Offline');
    expect(rows[1]).toHaveTextContent('0 of 2');
    expect(rows[2]).toHaveTextContent('0.1.0-dev');
    expect(rows[2]).toHaveTextContent('+3 ms');
    expect(rows[3]).toHaveTextContent('+2.60 s');

    const pool = screen.getByRole('region', { name: 'Pool 1' });
    expect(pool).toHaveTextContent('read quorum 4, write quorum 5');
    const sets = within(pool).getByRole('table', { name: 'Erasure sets of pool 1' });
    expect(within(sets).getByText('Degraded')).toBeInTheDocument();
    expect(sets).toHaveTextContent('6 of 8');

    const drives = within(pool).getByRole('table', { name: 'Drives of pool 1' });
    const offlineDrives = within(drives)
      .getAllByRole('row')
      .filter((r) => r.getAttribute('data-state') === 'offline');
    // The node row and its two drives.
    expect(offlineDrives).toHaveLength(3);
    expect(within(drives).getAllByText('node offline')).toHaveLength(2);
    expect(within(drives).getByText('Healing')).toBeInTheDocument();
    expect(drives).toHaveTextContent('128 objects healed (3 GiB), 0 failed');

    expect(screen.getByRole('region', { name: 'Health' })).toHaveTextContent('Degraded');
    expect(screen.getByRole('region', { name: 'Nodes online' })).toHaveTextContent('3 of 4');
    expect(screen.getByRole('region', { name: 'Drives online' })).toHaveTextContent(
      '1 drive healing',
    );
    expect(screen.getByRole('region', { name: 'Max clock skew' })).toHaveTextContent('2.60 s');
  });

  it('shows the single drive of a single-node deployment', async () => {
    renderWithProviders(<ClusterPage />, {
      services: createMockServices({ initialAccessKey: 'nexoadmin' }),
    });
    const pool = await screen.findByRole('region', { name: 'Pool 1' });
    expect(pool).toHaveTextContent('1 drive without erasure coding.');
    expect(within(pool).getByText('Healthy')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Deployment mode' })).toHaveTextContent(
      'Single node, single drive',
    );
  });
});

// covers: D-CLU-03
describe('ClusterPage pool management (D-CLU-03)', () => {
  const twoPools = {
    pools: [
      { index: 1, id: '/srv/p1/d{1...4}', total: 4000, free: 1000, draining: false },
      { index: 2, id: '/srv/p2/d{1...4}', total: 4000, free: 3000, draining: false },
    ],
  };

  it('decommissions a pool after confirmation and tells to remove it once complete', async () => {
    const user = userEvent.setup();
    const services = createMockServices({
      info: sampleClusterInfo(),
      pools: twoPools,
      poolPolls: 1,
      initialAccessKey: 'nexoadmin',
    });
    renderWithProviders(<ClusterPage />, { services });
    const section = await screen.findByRole('region', { name: 'Pool management' });
    const table = await within(section).findByRole('table', { name: 'Pool management' });
    // The first pool holds the system documents: no decommission.
    expect(
      within(table).queryByRole('button', { name: 'Decommission pool 1' }),
    ).not.toBeInTheDocument();
    await user.click(await within(table).findByRole('button', { name: 'Decommission pool 2' }));
    const dialog = await screen.findByRole('dialog', { name: 'Decommission pool 2?' });
    await user.click(within(dialog).getByRole('button', { name: 'Decommission' }));

    expect(
      await within(table).findByRole('button', { name: 'Cancel decommission of pool 2' }),
    ).toBeInTheDocument();
    expect(table).toHaveTextContent('Draining');
    expect(
      await within(section).findByText(/A decommission is complete/, {}, { timeout: 5000 }),
    ).toBeInTheDocument();
    expect(table).toHaveTextContent('Complete');
    expect(table).toHaveTextContent('1 object (1,000 B) moved');
  });

  it('starts and stops the rebalance', async () => {
    const user = userEvent.setup();
    const services = createMockServices({
      info: sampleClusterInfo(),
      pools: twoPools,
      poolPolls: 100,
      initialAccessKey: 'nexoadmin',
    });
    renderWithProviders(<ClusterPage />, { services });
    const section = await screen.findByRole('region', { name: 'Pool management' });
    await user.click(await within(section).findByRole('button', { name: 'Start rebalance' }));
    await user.click(await within(section).findByRole('button', { name: 'Stop rebalance' }));
    expect(await within(section).findByText('Canceled')).toBeInTheDocument();
    expect(await services.pools.list()).toMatchObject({ rebalance: { state: 'canceled' } });
  });

  it('hides the pool management in single-drive mode', async () => {
    renderWithProviders(<ClusterPage />, {
      services: createMockServices({ initialAccessKey: 'nexoadmin' }),
    });
    await screen.findByRole('region', { name: 'Pool 1' });
    expect(screen.queryByRole('region', { name: 'Pool management' })).not.toBeInTheDocument();
  });
});
