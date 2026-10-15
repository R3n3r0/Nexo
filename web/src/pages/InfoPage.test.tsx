import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { createMockServices, sampleServerInfo } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { InfoPage } from './InfoPage';

// covers: D-CFG-02
describe('InfoPage (D-CFG-02)', () => {
  it('shows version, build, deployment ID and mode', async () => {
    const info = sampleServerInfo({ version: '2.0.1', commit: 'deadbeef', mode: 'snmd' });
    renderWithProviders(<InfoPage />, {
      services: createMockServices({ info, initialAccessKey: 'nexoadmin' }),
    });
    const table = await screen.findByRole('table', { name: 'Server information' });
    expect(within(table).getByText('2.0.1')).toBeInTheDocument();
    expect(within(table).getByText('deadbeef')).toBeInTheDocument();
    expect(within(table).getByText(info.deploymentId)).toBeInTheDocument();
    expect(within(table).getByText('Single node, multiple drives')).toBeInTheDocument();
    expect(within(table).getByText('1 pools, 1 nodes, 1 drives')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy the deployment ID' })).toBeInTheDocument();
    expect(screen.getByRole('table', { name: 'Pools' })).toHaveTextContent('None (single drive)');
  });
});
