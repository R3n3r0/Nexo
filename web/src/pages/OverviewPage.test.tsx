import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { ApiError } from '@/services';
import { createMockServices, sampleServerInfo } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { summariseServerInfo } from '@/lib/serverInfo';

import { OverviewPage } from './OverviewPage';

describe('OverviewPage', () => {
  it('renders the stat tiles from InfoService', async () => {
    const info = sampleServerInfo({ version: '1.4.2', buckets: 42, objects: 12_840 });
    renderWithProviders(<OverviewPage />, {
      services: createMockServices({ info, initialAccessKey: 'nexoadmin' }),
    });
    expect(await screen.findByText('1.4.2')).toBeInTheDocument();
    expect(screen.getByText('Single node, single drive')).toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();
    expect(screen.getByText('12.8K')).toBeInTheDocument();
    expect(screen.getByText('194 GiB')).toBeInTheDocument();
    expect(screen.getByText('3d 4h')).toBeInTheDocument();
    expect(screen.getAllByText('1 of 1')).toHaveLength(2);
  });

  it('shows skeletons while loading', () => {
    renderWithProviders(<OverviewPage />, { services: createMockServices({ latencyMs: 1000 }) });
    expect(screen.getAllByTestId('stat-skeleton').length).toBeGreaterThan(0);
  });

  it('shows an error with a retry action when the request fails', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const failing = {
      ...services,
      info: {
        serverInfo: () =>
          Promise.reject(
            new ApiError({ title: 'Service Unavailable', status: 503, code: 'read_quorum' }),
          ),
      },
    };
    renderWithProviders(<OverviewPage />, { services: failing });
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load the server information.',
    );
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });
});

describe('summariseServerInfo', () => {
  it('counts nodes and drives that are online', () => {
    const info = sampleServerInfo();
    const offlineNode = structuredClone(info.nodes[0]);
    if (!offlineNode || !offlineNode.disks[0]) throw new Error('fixture');
    offlineNode.state = 'offline';
    offlineNode.disks[0].state = 'offline';
    offlineNode.disks.push({ ...offlineNode.disks[0], state: 'online', diskIdx: 1 });
    const summary = summariseServerInfo({ ...info, nodes: [...info.nodes, offlineNode] });
    expect(summary).toEqual({ nodes: 2, nodesOnline: 1, drives: 3, drivesOnline: 2 });
  });
});
