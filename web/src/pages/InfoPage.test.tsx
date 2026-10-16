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

  it('shows the license and links the source code of the build (AGPL-3.0 §13)', async () => {
    const info = sampleServerInfo({ version: 'v1.0.0', commit: 'deadbeef' });
    renderWithProviders(<InfoPage />, {
      services: createMockServices({ info, initialAccessKey: 'nexoadmin' }),
    });
    const table = await screen.findByRole('table', { name: 'Server information' });
    expect(within(table).getByRole('link', { name: 'AGPL-3.0-only' })).toHaveAttribute(
      'href',
      'https://www.gnu.org/licenses/agpl-3.0.html',
    );
    expect(within(table).getByText(/Powered by Nexo/)).toBeInTheDocument();
    expect(
      within(table).getByRole('link', { name: 'https://github.com/R3n3r0/Nexo/tree/v1.0.0' }),
    ).toHaveAttribute('href', 'https://github.com/R3n3r0/Nexo/tree/v1.0.0');
  });
});
