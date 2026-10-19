import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';
import { UploadProvider } from '@/upload/UploadProvider';

import { ObjectBrowser } from './ObjectBrowser';

const routes = [
  {
    path: '/buckets/:bucket/browse/*',
    element: (
      <UploadProvider>
        <ObjectBrowser />
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

function services(options: Parameters<typeof createMockServices>[0] = {}) {
  return createMockServices({
    initialAccessKey: 'nexoadmin',
    buckets: [bucket],
    objects: {
      media: [
        { key: 'videos/intro.mp4', contentType: 'video/mp4', size: 10 },
        { key: 'notes.txt', contentType: 'text/plain', content: 'x' },
      ],
    },
    ...options,
  });
}

async function openVideoTab(svc: ReturnType<typeof services>) {
  renderRoutes(routes, {
    initialEntries: ['/buckets/media/browse/?object=videos%2Fintro.mp4'],
    services: svc,
  });
  const drawer = await screen.findByRole('dialog', { name: 'intro.mp4' });
  const user = userEvent.setup();
  await user.click(await within(drawer).findByRole('tab', { name: 'Video' }));
  return { drawer, user };
}

beforeEach(() => {
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(1000);
});

describe('ObjectMediaPanel', () => {
  it('reprocesses a video and shows its outputs', async () => {
    const svc = services();
    await svc.media.set('media', {
      rules: [{ id: 'videos', renditions: ['720p', '480p'], poster: true }],
    });
    const reprocess = vi.spyOn(svc.media, 'reprocess');
    const { drawer, user } = await openVideoTab(svc);
    expect(await within(drawer).findByText('Not processed')).toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Reprocess' }));
    expect(await within(drawer).findByText('Processed')).toBeInTheDocument();
    expect(reprocess).toHaveBeenCalledWith('media', { key: 'videos/intro.mp4' });
    const table = within(drawer).getByRole('table', { name: 'Renditions' });
    expect(within(table).getByText('720p')).toBeInTheDocument();
    expect(within(table).getByText('1280×720')).toBeInTheDocument();
    expect(within(drawer).getByText('Rule videos')).toBeInTheDocument();
    // The demo serves no poster.
    expect(within(drawer).queryByRole('img')).toBeNull();
  });

  it('shows the failure, the stale outputs and the poster', async () => {
    const svc = services();
    vi.spyOn(svc.media, 'status').mockResolvedValue({
      state: 'failed',
      current: false,
      error: 'ffmpeg: invalid data',
      poster: true,
    });
    const { drawer } = await openVideoTab(svc);
    expect(await within(drawer).findByText('Failed')).toBeInTheDocument();
    expect(within(drawer).getByText('Outdated')).toBeInTheDocument();
    expect(within(drawer).getByText('ffmpeg: invalid data')).toBeInTheDocument();
    // A stale poster is not shown.
    expect(within(drawer).queryByRole('img')).toBeNull();
  });

  it('shows the poster of a processed video', async () => {
    const svc = services();
    vi.spyOn(svc.media, 'status').mockResolvedValue({
      state: 'done',
      current: true,
      poster: true,
      thumbnails: { interval: 10, width: 160, height: 90, columns: 10, count: 12 },
    });
    vi.spyOn(svc.objects, 'posterUrl').mockReturnValue('/poster.jpg');
    const { drawer } = await openVideoTab(svc);
    const poster = await within(drawer).findByRole('img', { name: 'Poster of intro.mp4' });
    expect(poster).toHaveAttribute('src', '/poster.jpg');
    expect(within(drawer).getByText('12 timeline thumbnails, every 10 s')).toBeInTheDocument();
  });

  it('offers the video tab for videos only', async () => {
    renderRoutes(routes, {
      initialEntries: ['/buckets/media/browse/?object=notes.txt'],
      services: services(),
    });
    const drawer = await screen.findByRole('dialog', { name: 'notes.txt' });
    await waitFor(() => {
      expect(within(drawer).getByRole('tab', { name: 'Preview' })).toBeInTheDocument();
    });
    expect(within(drawer).queryByRole('tab', { name: 'Video' })).toBeNull();
  });
});
