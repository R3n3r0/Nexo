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

/** The video rules of the media worker. */
describe('BucketMediaTab', () => {
  it('adds a rule with renditions, poster and thumbnails', async () => {
    const { user, services } = open();
    expect(
      await screen.findByText('No rule: the videos of this bucket are streamed as uploaded.'),
    ).toBeInTheDocument();
    await user.click(await enabledButton('Add rule'));
    const dialog = await screen.findByRole('dialog', { name: 'New video rule' });
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
    const table = screen.getByRole('table', { name: 'Video rules' });
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
    const dialog = await screen.findByRole('dialog', { name: 'New video rule' });
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
    const dialog = await screen.findByRole('dialog', { name: 'New video rule' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Rule ID' }), 'v');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(await within(dialog).findByText('worker said no (InvalidArgument)')).toBeInTheDocument();
  });

  it('edits and deletes a rule', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    await services.media.set('media', { rules: [{ id: 'v', poster: true }] });
    const { user } = open(services);
    await user.click(await screen.findByRole('button', { name: 'Edit rule v' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit the video rule' });
    expect(within(dialog).getByRole('textbox', { name: 'Rule ID' })).toHaveValue('v');
    await user.click(within(dialog).getByRole('textbox', { name: 'Renditions' }));
    await user.click(await screen.findByRole('option', { name: '360p' }));
    await user.keyboard('{Escape}');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    await waitFor(async () => {
      expect((await services.media.get('media')).rules[0]?.renditions).toEqual(['360p']);
    });

    await user.click(await screen.findByRole('button', { name: 'Delete rule v' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete the video rule' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(async () => {
      expect((await services.media.get('media')).rules).toEqual([]);
    });
    expect(
      await screen.findByText('No rule: the videos of this bucket are streamed as uploaded.'),
    ).toBeInTheDocument();
  });

  it('tells a user without admin:GetBucketMedia that the rules are hidden', async () => {
    open(readOnly());
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'You may not read the video rules of this bucket (admin:GetBucketMedia).',
    );
  });
});
