import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useLocation } from 'react-router';
import { describe, expect, it, vi } from 'vitest';

import { ApiError, type Bucket } from '@/services';
import { createMockServices, sampleBuckets } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { BucketsPage, sortBuckets } from './BucketsPage';

function Location() {
  const location = useLocation();
  return <div data-testid="location">{location.pathname}</div>;
}

const routes = [
  { path: '/buckets', element: <BucketsPage /> },
  { path: '/buckets/:bucket/*', element: <Location /> },
];

/** Mock services signed in as the root account. */
function adminServices(options: Parameters<typeof createMockServices>[0] = {}) {
  return createMockServices({ initialAccessKey: 'nexoadmin', ...options });
}

/** A user with the readonly policy (s3:GetObject only). */
function readOnlyServices() {
  return createMockServices({
    accounts: { nexoadmin: 'nexoadmin', reader: 'reader-secret' },
    users: [
      {
        accessKey: 'reader',
        status: 'enabled',
        policyNames: ['readonly'],
        groups: [],
        createdAt: '2026-09-01T00:00:00Z',
        updatedAt: '2026-09-01T00:00:00Z',
      },
    ],
    groups: [],
    initialAccessKey: 'reader',
  });
}

function rowNames(): string[] {
  const table = screen.getByRole('table', { name: 'Buckets' });
  return within(table)
    .getAllByRole('row')
    .slice(1)
    .map((row) => within(row).getAllByRole('cell')[0]?.querySelector('a')?.textContent ?? '');
}

describe('sortBuckets', () => {
  const buckets: Bucket[] = sampleBuckets();
  it('sorts by every column and filters by name', () => {
    expect(sortBuckets(buckets, '', 'name', 'asc').map((b) => b.name)).toEqual([
      'backups',
      'logs-archive',
      'media',
    ]);
    expect(sortBuckets(buckets, '', 'size', 'desc').map((b) => b.name)).toEqual([
      'backups',
      'media',
      'logs-archive',
    ]);
    expect(sortBuckets(buckets, '', 'objects', 'asc').map((b) => b.name)).toEqual([
      'logs-archive',
      'backups',
      'media',
    ]);
    expect(sortBuckets(buckets, '', 'created', 'desc').map((b) => b.name)).toEqual([
      'logs-archive',
      'media',
      'backups',
    ]);
    expect(sortBuckets(buckets, 'MED', 'name', 'asc').map((b) => b.name)).toEqual(['media']);
  });
});

// covers: D-BKT-01
describe('BucketsPage (TD-U-03)', () => {
  it('shows the buckets with size, objects and the public badge, and sorts them', async () => {
    const services = adminServices();
    await services.buckets.setAccess('media', { prefix: '', preset: 'download' });
    renderRoutes(routes, { initialEntries: ['/buckets'], services });

    expect(await screen.findByRole('link', { name: 'media' })).toBeInTheDocument();
    expect(rowNames()).toEqual(['backups', 'logs-archive', 'media']);
    expect(screen.getByText('Public')).toBeInTheDocument();
    expect(screen.getByText('120 GiB')).toBeInTheDocument();
    expect(screen.getByText('11,500')).toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Size' }));
    expect(rowNames()).toEqual(['backups', 'media', 'logs-archive']);
    expect(screen.getByRole('columnheader', { name: 'Size' })).toHaveAttribute(
      'aria-sort',
      'descending',
    );
    await user.click(screen.getByRole('button', { name: 'Size' }));
    expect(rowNames()).toEqual(['logs-archive', 'media', 'backups']);
    expect(screen.getByRole('columnheader', { name: 'Size' })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );
  });

  it('shows no usage for a bucket the user may list only in part', async () => {
    const services = adminServices();
    const media = sampleBuckets().find((b) => b.name === 'media');
    if (media === undefined) throw new Error('no sample bucket media');
    vi.spyOn(services.buckets, 'list').mockResolvedValue([
      { ...media, size: 0, objects: 0, partial: true },
    ]);
    renderRoutes(routes, { initialEntries: ['/buckets'], services });
    const row = (await screen.findByRole('link', { name: 'media' })).closest('tr');
    if (row === null) throw new Error('no row');
    expect(within(row).getByText('You may use part of this bucket only.')).toBeInTheDocument();
    expect(within(row).queryByText('0 B')).not.toBeInTheDocument();
  });

  it('filters by name and tells when nothing matches', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets'], services: adminServices() });
    await screen.findByRole('link', { name: 'media' });
    const user = userEvent.setup();
    await user.type(screen.getByRole('textbox', { name: 'Search buckets' }), 'back');
    expect(rowNames()).toEqual(['backups']);
    await user.type(screen.getByRole('textbox', { name: 'Search buckets' }), 'zzz');
    expect(screen.getByText('No bucket matches “backzzz”.')).toBeInTheDocument();
  });

  it('shows an empty state', async () => {
    renderRoutes(routes, {
      initialEntries: ['/buckets'],
      services: adminServices({ buckets: [] }),
    });
    expect(await screen.findByText('No buckets yet.')).toBeInTheDocument();
    expect(
      await screen.findByText('Create a bucket to start storing objects.'),
    ).toBeInTheDocument();
  });

  it('shows skeleton rows while loading', () => {
    renderRoutes(routes, {
      initialEntries: ['/buckets'],
      services: adminServices({ latencyMs: 10_000 }),
    });
    expect(screen.getAllByTestId('bucket-skeleton')).toHaveLength(3);
  });

  it('shows an error with a retry action', async () => {
    const services = adminServices();
    const list = vi
      .spyOn(services.buckets, 'list')
      .mockRejectedValueOnce(
        new ApiError({ title: 'Service Unavailable', status: 503, code: 'read_quorum' }),
      );
    renderRoutes(routes, { initialEntries: ['/buckets'], services });
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not load the buckets.');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByRole('link', { name: 'media' })).toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('deletes a bucket after typing its name, offering force for a non-empty one', async () => {
    const services = adminServices();
    renderRoutes(routes, { initialEntries: ['/buckets'], services });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Actions for media' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Delete' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete bucket media' });
    const confirm = within(dialog).getByRole('button', { name: 'Delete' });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox'), 'media');
    await user.click(confirm);
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('The bucket is not empty');
    await user.click(within(dialog).getByRole('checkbox', { name: /Delete all objects too/ }));
    await user.click(confirm);
    await waitFor(() => {
      expect(screen.queryByRole('link', { name: 'media' })).not.toBeInTheDocument();
    });
    await expect(services.buckets.get('media')).rejects.toMatchObject({ code: 'bucket_not_found' });
  });
});

// covers: D-BKT-01
describe('CreateBucketDialog (TD-U-04)', () => {
  it('validates the name live, sends the options and opens the new bucket', async () => {
    const services = adminServices();
    const create = vi.spyOn(services.buckets, 'create');
    renderRoutes(routes, { initialEntries: ['/buckets'], services });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Create bucket' }));
    const dialog = await screen.findByRole('dialog', { name: 'Create a bucket' });
    const name = within(dialog).getByRole('textbox', { name: /Name/ });

    await user.type(name, 'My_Bucket');
    expect(
      within(dialog).getByText('Use only lowercase letters, digits, dots and hyphens.'),
    ).toBeInTheDocument();
    await user.clear(name);
    await user.type(name, 'ab');
    expect(
      within(dialog).getByText('The name must be 3 to 63 characters long.'),
    ).toBeInTheDocument();
    await user.clear(name);
    await user.type(name, '10.0.0.1');
    expect(
      within(dialog).getByText('The name cannot look like an IP address.'),
    ).toBeInTheDocument();
    await user.clear(name);
    await user.type(name, 'photos-2026');
    expect(within(dialog).queryByText(/The name/)).not.toBeInTheDocument();

    await user.click(within(dialog).getByRole('switch', { name: /Object lock/ }));
    expect(within(dialog).getByRole('switch', { name: /Versioning/ })).toBeChecked();
    await user.type(within(dialog).getByRole('textbox', { name: 'Quota' }), '5');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent('/buckets/photos-2026/browse/');
    });
    expect(create).toHaveBeenCalledWith({
      name: 'photos-2026',
      versioning: true,
      objectLock: true,
      quotaBytes: 5 * 1024 ** 3,
    });
  });

  it('shows BucketAlreadyExists on the name field', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets'], services: adminServices() });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Create bucket' }));
    const dialog = await screen.findByRole('dialog', { name: 'Create a bucket' });
    await user.type(within(dialog).getByRole('textbox', { name: /Name/ }), 'media');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(
      await within(dialog).findByText('A bucket with this name already exists.'),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('textbox', { name: /Name/ })).toHaveAttribute(
      'aria-invalid',
      'true',
    );
  });
});

// covers: D-UX-02
describe('BucketsPage permissions (TD-U-09)', () => {
  it('hides creation and disables deletion for a read-only user', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets'], services: readOnlyServices() });
    expect(await screen.findByRole('link', { name: 'media' })).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'Create bucket' })).not.toBeInTheDocument();
    });
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Actions for media' }));
    const item = await screen.findByRole('menuitem', { name: 'Delete (not allowed)' });
    expect(item).toBeDisabled();
  });
});
