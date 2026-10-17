import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { summariseServerInfo } from '@/lib/serverInfo';
import { ApiError } from '@/services';
import { createMockServices, sampleClusterInfo, sampleServerInfo } from '@/services/mock';
import { renderRoutes, renderWithProviders } from '@/test/render';

import { capacityOf, OverviewPage, topBuckets } from './OverviewPage';

const overviewRoute = [{ path: '/', element: <OverviewPage /> }];

// covers: D-DASH-01
describe('OverviewPage (D-DASH-01)', () => {
  it('summarizes the health of a cluster and links to the cluster page', async () => {
    renderRoutes(overviewRoute, {
      services: createMockServices({ info: sampleClusterInfo(), initialAccessKey: 'nexoadmin' }),
    });
    const status = await screen.findByRole('region', { name: 'Cluster health' });
    expect(within(status).getByText('Degraded')).toBeInTheDocument();
    expect(status).toHaveTextContent(
      'Nodes online: 3 of 4 · drives online: 6 of 8 · healthy erasure sets: 0 of 1',
    );
    expect(status).toHaveTextContent('1 node offline');
    expect(within(status).getByText('nexo2:9000')).toBeInTheDocument();
    expect(status).toHaveTextContent(
      '1 drive(s) healing, 2 object(s) waiting for the background healing.',
    );
    expect(status).toHaveTextContent('Clock skew up to 2.60 s on nexo4:9000.');
    expect(within(status).getByRole('link', { name: 'Open the cluster page' })).toHaveAttribute(
      'href',
      '/cluster',
    );
    expect(screen.getByText('Cluster')).toBeInTheDocument();
    expect(screen.getByText(/^Distributed cluster/)).toBeInTheDocument();
  });

  it('renders the stat tiles from InfoService', async () => {
    const info = sampleServerInfo({ version: '1.4.2', buckets: 42, objects: 12_840 });
    renderRoutes(overviewRoute, {
      services: createMockServices({ info, initialAccessKey: 'nexoadmin' }),
    });
    expect(await screen.findByText('1.4.2')).toBeInTheDocument();
    expect(screen.getByText('Single drive')).toBeInTheDocument();
    expect(screen.getByText(/^Single node, single drive/)).toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();
    expect(screen.getByText('12.8K')).toBeInTheDocument();
    expect(
      within(screen.getByRole('region', { name: 'Used storage' })).getByText('194 GiB'),
    ).toBeInTheDocument();
    expect(screen.getByText('3d 4h')).toBeInTheDocument();
    expect(screen.getAllByText('1 of 1')).toHaveLength(2);
  });

  it('shows the capacity, the drives and the usage per bucket', async () => {
    renderRoutes(overviewRoute, {
      services: createMockServices({ initialAccessKey: 'nexoadmin' }),
    });
    const capacity = await screen.findByRole('region', { name: 'Capacity' });
    expect(within(capacity).getByText('38% used')).toBeInTheDocument();
    expect(within(capacity).getByText('318 GiB')).toBeInTheDocument();
    expect(within(capacity).getByText('512 GiB')).toBeInTheDocument();
    expect(
      within(capacity).getByRole('progressbar', { name: 'Used capacity of the drives' }),
    ).toBeInTheDocument();

    const nodes = screen.getByRole('region', { name: 'Nodes and drives' });
    expect(within(nodes).getByText('http://localhost:9000')).toBeInTheDocument();
    expect(within(nodes).getByText('/data')).toBeInTheDocument();
    expect(within(nodes).getAllByText('Online')).toHaveLength(2);
    expect(within(nodes).getByText('194 GiB of 512 GiB')).toBeInTheDocument();

    expect(await screen.findByRole('region', { name: 'Storage per bucket' })).toBeInTheDocument();
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
        ...services.info,
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

// covers: D-UX-02
describe('OverviewPage without admin:ServerInfo (TD-U-09)', () => {
  it('points to the buckets instead of failing', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const forbidden = {
      ...services,
      info: {
        ...services.info,
        serverInfo: () =>
          Promise.reject(new ApiError({ title: 'Forbidden', status: 403, code: 'access_denied' })),
      },
    };
    renderRoutes([{ path: '/', element: <OverviewPage /> }], { services: forbidden });
    expect(await screen.findByText(/cannot read the server status/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to the buckets' })).toHaveAttribute(
      'href',
      '/buckets',
    );
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});

describe('overview helpers', () => {
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

  it('sums the raw capacity of the drives', () => {
    expect(capacityOf(sampleServerInfo())).toEqual({
      total: 512 * 1024 ** 3,
      used: 194 * 1024 ** 3,
      free: 318 * 1024 ** 3,
    });
  });

  it('keeps the ten largest buckets', () => {
    const buckets = Object.fromEntries(
      Array.from({ length: 12 }, (_, i) => [
        `b${String(i)}`,
        { size: i * 10, objectsCount: 1, versionsCount: 1, deleteMarkers: 0 },
      ]),
    );
    const top = topBuckets({
      lastUpdate: '2026-10-08T00:00:00Z',
      objectsCount: 12,
      versionsCount: 12,
      totalSize: 660,
      bucketsCount: 12,
      buckets,
    });
    expect(top).toHaveLength(10);
    expect(top[0]).toEqual({ bucket: 'b11', size: 110 });
    expect(top[9]).toEqual({ bucket: 'b2', size: 20 });
  });
});
