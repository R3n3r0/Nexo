import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useLocation } from 'react-router';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '@/services';
import { createMockServices, type MockObjectSeed } from '@/services/mock';
import { renderRoutes } from '@/test/render';
import { UploadPanel } from '@/upload/UploadPanel';
import { UploadProvider } from '@/upload/UploadProvider';

import { ObjectBrowser } from './ObjectBrowser';
import { buildRows } from './rows';

function Location() {
  const location = useLocation();
  return <div data-testid="location">{`${location.pathname}${location.search}`}</div>;
}

const routes = [
  {
    path: '/buckets/:bucket/browse/*',
    element: (
      <UploadProvider>
        <Location />
        <ObjectBrowser />
        <UploadPanel />
      </UploadProvider>
    ),
  },
];

const bucket = {
  name: 'media',
  created: '2026-09-12T15:30:00Z',
  region: 'us-east-1',
  versioning: 'off' as const,
  objectLockEnabled: false,
  objects: 0,
  versions: 0,
  deleteMarkers: 0,
  size: 0,
  quotaBytes: 0,
  public: false,
  tags: {},
};

const seeds: MockObjectSeed[] = [
  { key: 'photos/2026/', size: 0 },
  { key: 'photos/2026/beach.png', contentType: 'image/png', size: 2048 },
  { key: 'photos/2026/città di notte.png', contentType: 'image/png', size: 4096 },
  { key: 'docs/report #1.pdf', contentType: 'application/pdf', size: 1000 },
  { key: 'docs/100% done.txt', contentType: 'text/plain', content: 'all done\n' },
  { key: 'a b#c%/inside.txt', contentType: 'text/plain', content: 'x' },
  { key: 'top.json', contentType: 'application/json', content: '{"a":1}' },
];

function services(options: Parameters<typeof createMockServices>[0] = {}) {
  return createMockServices({
    initialAccessKey: 'nexoadmin',
    buckets: [bucket],
    objects: { media: seeds },
    ...options,
  });
}

function readOnly() {
  return services({
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

function listing() {
  return screen.getByRole('table', { name: 'Objects' });
}

function names(): string[] {
  return within(listing())
    .getAllByRole('row')
    .slice(1)
    .map((row) => within(row).getAllByRole('cell')[1]?.textContent ?? '');
}

beforeEach(() => {
  // jsdom has no layout: give the virtualized viewport a size.
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(1000);
});

describe('buildRows', () => {
  it('puts folders first, deduplicates pages and filters by name', () => {
    const entry = (key: string) => ({
      key,
      size: 1,
      etag: 'e',
      lastModified: '2026-10-01T00:00:00Z',
      isDeleteMarker: false,
      isLatest: true,
    });
    const rows = buildRows([
      { objects: [entry('a/x.txt')], prefixes: ['a/b/'], isTruncated: true, nextCursor: 'c' },
      { objects: [entry('a/y.txt'), entry('a/x.txt')], prefixes: ['a/c/'], isTruncated: false },
    ]);
    expect(rows.map((r) => `${r.kind}:${r.name}`)).toEqual([
      'folder:b/',
      'folder:c/',
      'object:x.txt',
      'object:y.txt',
    ]);
    expect(
      buildRows([{ objects: [entry('a/X.txt')], prefixes: [], isTruncated: false }], 'x'),
    ).toHaveLength(1);
  });
});

describe('ObjectBrowser (TD-U-05)', () => {
  it('shows the folder of the route with a breadcrumb and navigates the folders', async () => {
    renderRoutes(routes, {
      initialEntries: ['/buckets/media/browse/photos/2026/'],
      services: services(),
    });
    expect(await screen.findByRole('button', { name: 'beach.png' })).toBeInTheDocument();
    expect(names()).toEqual(['beach.png', 'città di notte.png']);

    const crumbs = screen.getByRole('navigation', { name: 'Folder path' });
    expect(within(crumbs).getByText('2026')).toHaveAttribute('aria-current', 'page');
    const user = userEvent.setup();
    await user.click(within(crumbs).getByRole('link', { name: 'photos' }));
    expect(screen.getByTestId('location')).toHaveTextContent('/buckets/media/browse/photos/');
    expect(await screen.findByRole('link', { name: '2026/' })).toBeInTheDocument();

    await user.click(within(crumbs).getByRole('link', { name: 'media' }));
    expect(await screen.findByRole('link', { name: 'docs/' })).toBeInTheDocument();
    expect(names()).toEqual(['a b#c%/', 'docs/', 'photos/', 'top.json']);

    await user.click(screen.getByRole('link', { name: 'a b#c%/' }));
    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent(
        '/buckets/media/browse/a%20b%23c%25/',
      );
    });
    expect(await screen.findByRole('button', { name: 'inside.txt' })).toBeInTheDocument();
  });

  it('opens a deep link with special characters and the object drawer', async () => {
    renderRoutes(routes, {
      initialEntries: ['/buckets/media/browse/docs/?object=docs%2F100%25+done.txt'],
      services: services(),
    });
    const drawer = await screen.findByRole('dialog', { name: '100% done.txt' });
    expect(await within(drawer).findByText('docs/100% done.txt')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(drawer).getByRole('tab', { name: 'Preview' }));
    expect(await within(drawer).findByText('all done')).toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Close' }));
    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent(
        /^\/buckets\/media\/browse\/docs\/$/,
      );
    });
    await user.click(screen.getByRole('button', { name: 'report #1.pdf' }));
    await waitFor(() => {
      expect(
        new URLSearchParams(screen.getByTestId('location').textContent?.split('?')[1]).get(
          'object',
        ),
      ).toBe('docs/report #1.pdf');
    });
  });

  it('pages through a large folder with the cursor and renders only the visible rows', async () => {
    const many = Array.from({ length: 2500 }, (_, i) => ({
      key: `logs/${String(i).padStart(5, '0')}.log`,
    }));
    const svc = services({ objects: { media: many } });
    const list = vi.spyOn(svc.objects, 'list');
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/logs/'], services: svc });

    expect(await screen.findByRole('button', { name: '00000.log' })).toBeInTheDocument();
    expect(listing()).toHaveAttribute('aria-rowcount', '1001');
    // Virtualized: a window of rows, not a thousand.
    expect(within(listing()).getAllByRole('row').length).toBeLessThan(80);

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    await waitFor(() => {
      expect(listing()).toHaveAttribute('aria-rowcount', '2001');
    });
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    await waitFor(() => {
      expect(listing()).toHaveAttribute('aria-rowcount', '2501');
    });
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
    const cursors = list.mock.calls.map((c) => c[1]?.cursor);
    expect(cursors[0]).toBeUndefined();
    expect(cursors.slice(1).every((c) => typeof c === 'string' && c !== '')).toBe(true);
    expect(screen.getByText('2500 items loaded')).toBeInTheDocument();
  });

  it('virtualizes ten thousand rows', async () => {
    const many = Array.from({ length: 10_000 }, (_, i) => ({
      key: `k${String(i).padStart(5, '0')}`,
    }));
    const svc = services({ objects: { media: many } });
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: svc });
    expect(await screen.findByRole('button', { name: 'k00000' })).toBeInTheDocument();
    const user = userEvent.setup();
    for (let loaded = 1000; loaded < 10_000; loaded += 1000) {
      await user.click(screen.getByRole('button', { name: 'Load more' }));
      await waitFor(() => {
        expect(listing()).toHaveAttribute('aria-rowcount', String(loaded + 1001));
      });
    }
    expect(within(listing()).getAllByRole('row').length).toBeLessThan(80);
  });

  it('selects several rows and deletes a folder after a typed confirmation', async () => {
    const svc = services();
    const remove = vi.spyOn(svc.objects, 'remove');
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: svc });
    await screen.findByRole('link', { name: 'docs/' });
    const user = userEvent.setup();
    await user.click(screen.getByRole('checkbox', { name: 'Select docs/' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select top.json' }));
    expect(screen.getByText('2 of 4 selected')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Download (2)' })).toBeEnabled();

    await user.click(screen.getByRole('button', { name: 'Delete (2)' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete 2 items' });
    const confirm = within(dialog).getByRole('button', { name: 'Delete' });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox'), 'docs/');
    await user.click(confirm);

    await waitFor(() => {
      expect(names()).toEqual(['a b#c%/', 'photos/']);
    });
    expect(remove).toHaveBeenCalledWith('media', {
      objects: [{ key: 'top.json' }],
      prefixes: ['docs/'],
    });
  });

  it('selects every loaded row', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: services() });
    await screen.findByRole('link', { name: 'docs/' });
    const user = userEvent.setup();
    await user.click(screen.getByRole('checkbox', { name: 'Select all loaded rows' }));
    expect(screen.getByText('4 of 4 selected')).toBeInTheDocument();
    await user.click(screen.getByRole('checkbox', { name: 'Select all loaded rows' }));
    expect(screen.getByText('4 items loaded')).toBeInTheDocument();
  });

  it('creates a folder and refreshes the listing (TD-U-14)', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/docs/'], services: services() });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New folder' }));
    const dialog = await screen.findByRole('dialog', { name: 'New folder' });
    await user.type(within(dialog).getByRole('textbox', { name: /Folder name/ }), '2026 #q');
    expect(within(dialog).getByText('Creates media/docs/2026 #q/')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(await screen.findByRole('link', { name: '2026 #q/' })).toBeInTheDocument();
  });

  it('uploads picked files into the current folder and refreshes the listing', async () => {
    const svc = services();
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/docs/'], services: svc });
    await screen.findByRole('button', { name: 'report #1.pdf' });
    const file = new File(['hello'], 'new note.txt', { type: 'text/plain' });
    fireEvent.change(screen.getByTestId('upload-files-input'), { target: { files: [file] } });
    expect(await screen.findByRole('button', { name: 'new note.txt' })).toBeInTheDocument();
    expect(await svc.objects.stat('media', 'docs/new note.txt')).toMatchObject({ size: 5 });
    expect(screen.getByRole('region', { name: 'Uploads' })).toHaveTextContent('Completed');
  });

  it('moves an object', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: services() });
    await screen.findByRole('button', { name: 'top.json' });
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Actions for top.json' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Rename or move' }));
    const dialog = await screen.findByRole('dialog', { name: 'Move top.json' });
    const input = within(dialog).getByRole('textbox', { name: /New key/ });
    await user.clear(input);
    await user.type(input, 'docs/top.json');
    await user.click(within(dialog).getByRole('button', { name: 'Move' }));
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'top.json' })).not.toBeInTheDocument();
    });
  });

  it('opens the share dialog of an object (TD-U-10)', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: services() });
    await screen.findByRole('button', { name: 'top.json' });
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Actions for top.json' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Share' }));
    expect(await screen.findByRole('dialog')).toHaveTextContent('media/top.json');
  });
});

describe('ObjectBrowser permissions (TD-U-09)', () => {
  it('offers only reading to a read-only user', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: readOnly() });
    await screen.findByRole('button', { name: 'top.json' });
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Download folder as zip' })).toBeEnabled();
    });
    expect(screen.queryByRole('button', { name: 'Upload' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'New folder' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Delete/ })).not.toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Actions for top.json' }));
    expect(await screen.findByRole('menuitem', { name: 'Delete' })).toBeDisabled();
    expect(screen.getByRole('menuitem', { name: 'Rename or move' })).toBeDisabled();
    expect(screen.getByRole('menuitem', { name: 'Download' })).toBeEnabled();
  });
});

describe('ObjectBrowser home directory (docs/09 §9.3)', () => {
  it('opens a folder by path when the current one cannot be listed', async () => {
    const svc = services();
    const list = svc.objects.list.bind(svc.objects);
    // The requester may list photos/ only (a home directory): every other
    // listing answers 403, as the Admin API does.
    vi.spyOn(svc.objects, 'list').mockImplementation(async (name, query = {}) => {
      if ((query.prefix ?? '') !== 'photos/') {
        throw new ApiError({
          status: 403,
          code: 'access_denied',
          title: 'Forbidden',
          detail: 'no policy allows s3:ListBucket',
        });
      }
      return list(name, query);
    });
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: svc });
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('You may not list this folder.');
    const input = within(alert).getByLabelText('Folder path');
    expect(within(alert).getByText('For example home/nexoadmin/')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.clear(input);
    await user.type(input, 'photos');
    await user.click(within(alert).getByRole('button', { name: 'Open folder' }));
    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent('/buckets/media/browse/photos/');
    });
    expect(await screen.findByRole('link', { name: '2026/' })).toBeInTheDocument();
  });
});
