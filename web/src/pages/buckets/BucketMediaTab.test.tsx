import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { BucketMediaTab } from './BucketMediaTab';

const routes = [{ path: '/buckets/:bucket/media', element: <BucketMediaTab /> }];

function open(services = createMockServices({ initialAccessKey: 'nexoadmin' })) {
  const result = renderRoutes(routes, { initialEntries: ['/buckets/media/media'], services });
  return { ...result, user: userEvent.setup() };
}

async function enabledButton(name: string) {
  await waitFor(() => {
    expect(screen.getByRole('button', { name })).toBeEnabled();
  });
  return screen.getByRole('button', { name });
}

function readOnly() {
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

/** The video and image rules of the media worker. */
describe('BucketMediaTab', () => {
  it('adds a rule with renditions, poster and thumbnails', async () => {
    const { user, services } = open();
    expect(
      await screen.findByText(
        'No rule: the videos and images of this bucket are served as uploaded.',
      ),
    ).toBeInTheDocument();
    await user.click(await enabledButton('Add rule'));
    const dialog = await screen.findByRole('dialog', { name: 'New media rule' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Rule ID' }), 'clips');
    await user.type(within(dialog).getByRole('textbox', { name: 'Key prefix' }), 'clips/');
    await user.click(within(dialog).getByRole('textbox', { name: 'Renditions' }));
    await user.click(await screen.findByRole('option', { name: '1080p' }));
    await user.keyboard('{Escape}');
    await user.click(within(dialog).getByRole('switch', { name: /^Timeline thumbnails/ }));
    const interval = within(dialog).getByRole('textbox', { name: 'Thumbnail interval' });
    await user.clear(interval);
    await user.type(interval, '30s');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const table = screen.getByRole('table', { name: 'Media rules' });
    expect(within(table).getByText('clips')).toBeInTheDocument();
    expect(within(table).getByText('Prefix clips/')).toBeInTheDocument();
    expect(within(table).getByText('1080p')).toBeInTheDocument();
    expect(within(table).getByText('Poster')).toBeInTheDocument();
    expect(within(table).getByText('Thumbnails every 30s')).toBeInTheDocument();
    expect((await services.media.get('media')).rules).toEqual([
      {
        id: 'clips',
        prefix: 'clips/',
        suffixes: ['.m4v', '.mov', '.mp4'],
        renditions: ['1080p', '720p', '480p'],
        poster: true,
        thumbnails: { interval: '30s' },
      },
    ]);
  });

  it('validates the rule before sending', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const set = vi.spyOn(services.media, 'set');
    const { user } = open(services);
    await user.click(await enabledButton('Add rule'));
    const dialog = await screen.findByRole('dialog', { name: 'New media rule' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Rule ID' }), 'bad id');
    await user.type(within(dialog).getByRole('textbox', { name: 'Key prefix' }), '.nexo-media/');
    await user.click(within(dialog).getByRole('checkbox', { name: /^Poster frame/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(within(dialog).getAllByText('1-64 letters, digits, “.”, “_” or “-”.')).not.toHaveLength(
      0,
    );
    expect(
      within(dialog).getByText('The prefix .nexo-media/ is reserved for the outputs.'),
    ).toBeInTheDocument();
    expect(set).not.toHaveBeenCalled();
  });

  it('shows the problems the server reports', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const { ApiError } = await import('@/services');
    vi.spyOn(services.media, 'set').mockRejectedValue(
      new ApiError({
        title: 'Bad Request',
        status: 400,
        code: 'invalid_configuration',
        detail: 'x',
        ruleErrors: [{ rule: 1, id: 'v', code: 'InvalidArgument', message: 'worker said no' }],
      }),
    );
    const { user } = open(services);
    await user.click(await enabledButton('Add rule'));
    const dialog = await screen.findByRole('dialog', { name: 'New media rule' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Rule ID' }), 'v');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(await within(dialog).findByText('worker said no (InvalidArgument)')).toBeInTheDocument();
  });

  it('edits and deletes a rule', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    await services.media.set('media', { rules: [{ id: 'v', poster: true }] });
    const { user } = open(services);
    await user.click(await screen.findByRole('button', { name: 'Edit rule v' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit the media rule' });
    expect(within(dialog).getByRole('textbox', { name: 'Rule ID' })).toHaveValue('v');
    await user.click(within(dialog).getByRole('textbox', { name: 'Renditions' }));
    await user.click(await screen.findByRole('option', { name: '360p' }));
    await user.keyboard('{Escape}');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    await waitFor(async () => {
      expect((await services.media.get('media')).rules[0]?.renditions).toEqual(['360p']);
    });

    await user.click(await screen.findByRole('button', { name: 'Delete rule v' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete the media rule' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(async () => {
      expect((await services.media.get('media')).rules).toEqual([]);
    });
    expect(
      await screen.findByText(
        'No rule: the videos and images of this bucket are served as uploaded.',
      ),
    ).toBeInTheDocument();
  });

  it('adds an image rule with its formats and widths', async () => {
    const { user, services } = open();
    await user.click(await enabledButton('Add rule'));
    const dialog = await screen.findByRole('dialog', { name: 'New media rule' });
    await user.click(within(dialog).getByRole('radio', { name: 'Images' }));
    // An image rule has no video outputs; its suffixes default to the images.
    expect(within(dialog).queryByRole('textbox', { name: 'Renditions' })).toBeNull();
    expect(within(dialog).queryByRole('checkbox', { name: /^Poster frame/ })).toBeNull();
    expect(within(dialog).getByText(/\.avif, \.bmp, \.gif/)).toBeInTheDocument();
    await user.type(within(dialog).getByRole('textbox', { name: 'Rule ID' }), 'photos');
    await user.type(within(dialog).getByRole('textbox', { name: 'Key prefix' }), 'photos/');
    // All the formats and the default widths are proposed: drop PNG and 2560.
    const formats = within(dialog).getByRole('textbox', { name: 'Formats' });
    const formatsField = formats.closest('.mantine-MultiSelect-root') as HTMLElement;
    await user.click(within(formatsField).getByText('PNG').parentElement!.querySelector('button')!);
    const widths = within(dialog).getByRole('textbox', { name: 'Widths' });
    const widthsField = widths.closest('.mantine-TagsInput-root') as HTMLElement;
    await user.click(within(widthsField).getByText('2560').parentElement!.querySelector('button')!);
    await user.type(widths, '96{Enter}');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const table = screen.getByRole('table', { name: 'Media rules' });
    expect(within(table).getByText('Images')).toBeInTheDocument();
    expect(within(table).getByText('AVIF')).toBeInTheDocument();
    expect(within(table).getByText('JPEG')).toBeInTheDocument();
    expect(within(table).queryByText('PNG')).toBeNull();
    expect(within(table).getByText('96, 160, 320, 640, 1280, 1920 px')).toBeInTheDocument();
    expect((await services.media.get('media')).rules).toEqual([
      {
        id: 'photos',
        prefix: 'photos/',
        suffixes: [
          '.avif',
          '.bmp',
          '.gif',
          '.heic',
          '.heif',
          '.jpeg',
          '.jpg',
          '.png',
          '.tif',
          '.tiff',
          '.webp',
        ],
        images: { formats: ['avif', 'webp', 'jpeg'], widths: [96, 160, 320, 640, 1280, 1920] },
      },
    ]);
  });

  it('validates the widths of an image rule', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const set = vi.spyOn(services.media, 'set');
    const { user } = open(services);
    await user.click(await enabledButton('Add rule'));
    const dialog = await screen.findByRole('dialog', { name: 'New media rule' });
    await user.click(within(dialog).getByRole('radio', { name: 'Images' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Rule ID' }), 'img');
    await user.type(within(dialog).getByRole('textbox', { name: 'Widths' }), '8{Enter}');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(
      within(dialog).getByText('Each width must be a whole number of pixels from 16 to 8192.'),
    ).toBeInTheDocument();
    expect(set).not.toHaveBeenCalled();
  });

  it('edits an image rule as an image rule', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    await services.media.set('media', {
      rules: [{ id: 'img', images: { formats: ['webp'], widths: [320] } }],
    });
    const { user } = open(services);
    const table = await screen.findByRole('table', { name: 'Media rules' });
    expect(within(table).getByText('WebP')).toBeInTheDocument();
    expect(within(table).getByText('320 px')).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: 'Edit rule img' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit the media rule' });
    expect(within(dialog).getByRole('radio', { name: 'Images' })).toBeChecked();
    expect(within(dialog).getByRole('textbox', { name: 'Widths' })).toBeInTheDocument();
  });

  it('refuses invalid image rules with the messages of the server', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    await expect(
      services.media.set('media', {
        rules: [{ id: 'img', images: { formats: ['gif' as 'png'] } }],
      }),
    ).rejects.toMatchObject({
      ruleErrors: [
        expect.objectContaining({
          message: 'media rule "img": unknown image format "gif" (one of avif, webp, jpeg, png)',
        }),
      ],
    });
    await expect(
      services.media.set('media', { rules: [{ id: 'img', images: { widths: [9000] } }] }),
    ).rejects.toMatchObject({
      ruleErrors: [
        expect.objectContaining({
          message: 'media rule "img": the image widths must be from 16 to 8192 pixels, not 9000',
        }),
      ],
    });
    await expect(
      services.media.set('media', {
        rules: [{ id: 'img', images: { widths: Array.from({ length: 17 }, (_, i) => 20 + i) } }],
      }),
    ).rejects.toMatchObject({
      ruleErrors: [
        expect.objectContaining({ message: 'media rule "img": at most 16 image widths' }),
      ],
    });
    await expect(
      services.media.set('media', { rules: [{ id: 'img', poster: true, images: {} }] }),
    ).rejects.toMatchObject({
      ruleErrors: [
        expect.objectContaining({
          message: 'media rule "img": an image rule has no renditions, poster or thumbnails',
        }),
      ],
    });
  });

  it('tells a user without admin:GetBucketMedia that the rules are hidden', async () => {
    open(readOnly());
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'You may not read the media rules of this bucket (admin:GetBucketMedia).',
    );
  });
});
