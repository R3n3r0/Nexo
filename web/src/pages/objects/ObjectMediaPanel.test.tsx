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
        { key: 'photos/beach.png', contentType: 'image/png', size: 10 },
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
  await user.click(await within(drawer).findByRole('tab', { name: 'Media' }));
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

  it('offers the media tab for videos and images only', async () => {
    renderRoutes(routes, {
      initialEntries: ['/buckets/media/browse/?object=notes.txt'],
      services: services(),
    });
    const drawer = await screen.findByRole('dialog', { name: 'notes.txt' });
    await waitFor(() => {
      expect(within(drawer).getByRole('tab', { name: 'Preview' })).toBeInTheDocument();
    });
    expect(within(drawer).queryByRole('tab', { name: 'Media' })).toBeNull();
  });

  it('shows the variants of a processed image, with a preview and their URLs', async () => {
    const svc = services();
    await svc.media.set('media', {
      rules: [{ id: 'photos', images: { formats: ['webp', 'jpeg'], widths: [320, 640, 4096] } }],
    });
    renderRoutes(routes, {
      initialEntries: ['/buckets/media/browse/?object=photos%2Fbeach.png'],
      services: svc,
    });
    const drawer = await screen.findByRole('dialog', { name: 'beach.png' });
    const user = userEvent.setup();
    await user.click(await within(drawer).findByRole('tab', { name: 'Media' }));
    expect(await within(drawer).findByText('Not processed')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Reprocess' }));
    expect(await within(drawer).findByText('Processed')).toBeInTheDocument();
    expect(within(drawer).getByText('Image 1600×1200')).toBeInTheDocument();
    expect(within(drawer).getByText('Transparency')).toBeInTheDocument();

    // WebP then JPEG, each at 320 and 640 pixels and at its own size (the
    // 4096 width is wider than the image).
    const table = within(drawer).getByRole('table', { name: 'Variants' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringMatching(/^WebP320 px320×240/),
      expect.stringMatching(/^WebP640 px640×480/),
      expect.stringMatching(/^WebPOwn size1600×1200/),
      expect.stringMatching(/^JPEG320 px320×240/),
      expect.stringMatching(/^JPEG640 px640×480/),
      expect.stringMatching(/^JPEGOwn size1600×1200/),
    ]);
    expect(within(table).getByText('7.5 KiB')).toBeInTheDocument();

    // The WebP variant at most 640 pixels wide is previewed first.
    const preview = within(drawer).getByRole('img', { name: 'WebP variant of beach.png, 640×480' });
    expect(preview).toHaveAttribute(
      'src',
      '/api/v1/buckets/media/objects/image?format=webp&key=photos%2Fbeach.png&w=640',
    );
    await user.click(
      within(table).getByRole('button', { name: 'Preview the JPEG variant 1600×1200' }),
    );
    expect(
      within(drawer).getByRole('img', { name: 'JPEG variant of beach.png, 1600×1200' }),
    ).toHaveAttribute(
      'src',
      '/api/v1/buckets/media/objects/image?format=jpeg&key=photos%2Fbeach.png',
    );
    expect(
      within(table).getByRole('link', { name: 'Open the JPEG variant 320×240' }),
    ).toHaveAttribute(
      'href',
      '/api/v1/buckets/media/objects/image?format=jpeg&key=photos%2Fbeach.png&w=320',
    );
    await user.click(
      within(table).getByRole('button', { name: 'Copy the URL of the WebP variant 320×240' }),
    );
    await waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe(
        new URL(
          '/api/v1/buckets/media/objects/image?format=webp&key=photos%2Fbeach.png&w=320',
          window.location.href,
        ).href,
      );
    });
  });

  it('shows stale image variants without serving them', async () => {
    const svc = services();
    vi.spyOn(svc.media, 'status').mockResolvedValue({
      state: 'done',
      current: false,
      width: 800,
      height: 600,
      images: [
        {
          format: 'avif',
          width: 0,
          pixelWidth: 800,
          pixelHeight: 600,
          size: 20_000,
          contentType: 'image/avif',
          url: '/api/v1/buckets/media/objects/image?format=avif&key=photos%2Fbeach.png',
        },
      ],
    });
    renderRoutes(routes, {
      initialEntries: ['/buckets/media/browse/?object=photos%2Fbeach.png'],
      services: svc,
    });
    const drawer = await screen.findByRole('dialog', { name: 'beach.png' });
    const user = userEvent.setup();
    await user.click(await within(drawer).findByRole('tab', { name: 'Media' }));
    const table = await within(drawer).findByRole('table', { name: 'Variants' });
    expect(within(table).getByText('AVIF')).toBeInTheDocument();
    expect(within(drawer).getByText('Outdated')).toBeInTheDocument();
    expect(within(drawer).queryByRole('img')).toBeNull();
    expect(within(table).queryByRole('link')).toBeNull();
  });

  it('uploads, edits, downloads and removes the subtitles of a video', async () => {
    const svc = services();
    const { drawer, user } = await openVideoTab(svc);
    expect(await within(drawer).findByText(/No subtitles/)).toBeInTheDocument();

    // Upload a SubRip file as the Italian track.
    await user.click(within(drawer).getByRole('button', { name: 'Upload subtitles' }));
    const upload = await screen.findByRole('dialog', { name: 'Upload subtitles' });
    await user.type(within(upload).getByRole('textbox', { name: /Language/ }), 'xx_!');
    expect(within(upload).getByText('Not a language tag.')).toBeInTheDocument();
    await user.clear(within(upload).getByRole('textbox', { name: /Language/ }));
    await user.type(within(upload).getByRole('textbox', { name: /Language/ }), 'IT');
    const srt = new File(['1\n00:00:01,000 --> 00:00:02,500\nCiao a tutti\n'], 'it.srt', {
      type: 'application/x-subrip',
    });
    const input = upload.querySelector('input[type="file"]');
    if (!(input instanceof HTMLInputElement)) throw new Error('no file input');
    await user.upload(input, srt);
    await user.click(within(upload).getByRole('button', { name: 'Upload' }));
    const table = await within(drawer).findByRole('table', { name: 'Subtitles' });
    expect(within(table).getByText('Italiano')).toBeInTheDocument();
    expect(within(table).getByText('Manual')).toBeInTheDocument();

    // Edit: add a cue, an invalid time blocks the save.
    await user.click(within(table).getByRole('button', { name: 'Edit the Italiano subtitles' }));
    const editor = await screen.findByRole('dialog', { name: 'Italiano subtitles (it)' });
    const start = within(editor).getByRole('textbox', { name: 'Start of cue 1' });
    expect(start).toHaveValue('00:00:01.000');
    await user.click(within(editor).getByRole('button', { name: 'Add a cue' }));
    await user.type(within(editor).getByRole('textbox', { name: 'Text of cue 2' }), 'Benvenuti');
    await user.clear(within(editor).getByRole('textbox', { name: 'End of cue 2' }));
    await user.type(within(editor).getByRole('textbox', { name: 'End of cue 2' }), '1:2');
    expect(within(editor).getByRole('button', { name: 'Save' })).toBeDisabled();
    await user.clear(within(editor).getByRole('textbox', { name: 'End of cue 2' }));
    await user.type(within(editor).getByRole('textbox', { name: 'End of cue 2' }), '00:00:04.000');
    await user.click(within(editor).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: 'Italiano subtitles (it)' })).toBeNull();
    });
    const cues = await svc.media.subtitleCues('media', 'videos/intro.mp4', 'it');
    expect(cues.cues).toEqual([
      { start: 1, end: 2.5, text: 'Ciao a tutti' },
      { start: 2.5, end: 4, text: 'Benvenuti' },
    ]);
    await waitFor(() => {
      expect(within(table).getByText('2')).toBeInTheDocument();
    });

    // Download links for both formats.
    await user.click(
      within(table).getByRole('button', { name: 'Download the Italiano subtitles' }),
    );
    const srtLink = await screen.findByRole('menuitem', { name: 'SubRip (.srt)' });
    expect(decodeURIComponent(srtLink.getAttribute('href') ?? '')).toContain(
      '2\n00:00:02,500 --> 00:00:04,000\nBenvenuti',
    );

    // Remove.
    await user.keyboard('{Escape}');
    await user.click(within(table).getByRole('button', { name: 'Remove the Italiano subtitles' }));
    const confirm = await screen.findByRole('dialog', { name: 'Remove subtitles' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    expect(await within(drawer).findByText(/No subtitles/)).toBeInTheDocument();
  });

  it('shows why the automatic subtitles failed', async () => {
    const svc = services();
    vi.spyOn(svc.media, 'status').mockResolvedValue({
      state: 'done',
      current: true,
      subtitlesError: 'no transcription service is configured',
    });
    const { drawer } = await openVideoTab(svc);
    expect(await within(drawer).findByText('The automatic subtitles failed')).toBeInTheDocument();
    expect(within(drawer).getByText('no transcription service is configured')).toBeInTheDocument();
  });
});
