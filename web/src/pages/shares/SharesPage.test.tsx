import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { SharesPage } from './SharesPage';

const accounts = { nexoadmin: 'nexoadmin', alice: 'alice-secret' };

function renderPage(initialAccessKey = 'nexoadmin', path = '/shares') {
  const services = createMockServices({ accounts, initialAccessKey });
  const view = renderRoutes([{ path: '/shares', element: <SharesPage /> }], {
    initialEntries: [path],
    services,
  });
  return { ...view, services };
}

describe('Shares page (D-SHR-03)', () => {
  // Regression (D-SHR-01): the copy button of a prefix share copied the
  // ListObjectsV2 URL of the prefix, which downloads nothing.
  it('shows the per-object links of a shared prefix', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ accounts, initialAccessKey: 'nexoadmin' });
    await services.shares.create({ bucket: 'media', key: 'photos/2026/', prefix: true });
    renderRoutes([{ path: '/shares', element: <SharesPage /> }], {
      initialEntries: ['/shares'],
      services,
    });
    const table = await screen.findByRole('table', { name: 'Active links' });
    await within(table).findByText('media/photos/2026/');
    expect(
      within(table).queryByRole('button', { name: 'Copy the link of media/photos/2026/' }),
    ).not.toBeInTheDocument();
    await user.click(
      within(table).getByRole('button', { name: 'Links of the objects of media/photos/2026/' }),
    );
    const dialog = await screen.findByRole('dialog', {
      name: 'Shared objects of media/photos/2026/',
    });
    const links = await within(dialog).findByRole('table', { name: 'Links of the shared objects' });
    expect(within(links).getByRole('link', { name: 'photos/2026/beach.png' })).toBeInTheDocument();
    // Object shares keep their direct link.
    expect(
      within(table).getByRole('button', { name: 'Copy the link of media/videos/intro.mp4' }),
    ).toBeInTheDocument();
  });

  it('lists every link for an administrator and revokes a selection', async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    const table = await screen.findByRole('table', { name: 'Active links' });
    expect(await within(table).findByText('media/videos/intro.mp4')).toBeInTheDocument();
    expect(within(table).getByText('backups/incoming/')).toBeInTheDocument();

    await user.click(within(table).getByRole('checkbox', { name: 'Select all links' }));
    await user.click(screen.getByRole('button', { name: 'Revoke selected (2)' }));
    const confirm = await screen.findByRole('dialog', { name: 'Revoke share links' });
    expect(confirm).toHaveTextContent('Revoke 2 selected links?');
    await user.click(within(confirm).getByRole('button', { name: 'Revoke' }));
    expect(await screen.findByText('No active share links.')).toBeInTheDocument();
    expect(await services.shares.list()).toEqual([]);
  });

  it('revokes a single link and every link of a user', async () => {
    const user = userEvent.setup();
    const { services } = renderPage();
    const revokeAll = vi.spyOn(services.shares, 'revokeAll');
    await user.click(
      await screen.findByRole('button', { name: 'Revoke the link of media/videos/intro.mp4' }),
    );
    const confirm = await screen.findByRole('dialog', { name: 'Revoke share links' });
    await user.click(within(confirm).getByRole('button', { name: 'Revoke' }));
    await waitFor(() => {
      expect(screen.queryByText('media/videos/intro.mp4')).not.toBeInTheDocument();
    });

    await user.type(screen.getByRole('textbox', { name: 'Owner' }), 'alice');
    await user.click(await screen.findByRole('button', { name: 'Revoke all links of alice' }));
    const confirmAll = await screen.findByRole('dialog', { name: 'Revoke share links' });
    expect(confirmAll).toHaveTextContent('Revoke all the share links of alice?');
    await user.click(within(confirmAll).getByRole('button', { name: 'Revoke' }));
    await waitFor(() => {
      expect(revokeAll).toHaveBeenCalledWith('alice');
    });
  });

  it('shows a user only its own links (TD-P-07)', async () => {
    renderPage('alice');
    const table = await screen.findByRole('table', { name: 'Active links' });
    expect(await within(table).findByText('backups/incoming/')).toBeInTheDocument();
    expect(within(table).queryByText('media/videos/intro.mp4')).not.toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: 'Owner' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Revoke all my links' })).toBeInTheDocument();
  });

  it('generates a POST policy form with its snippets (D-SHR-02)', async () => {
    const user = userEvent.setup();
    const { services } = renderPage('nexoadmin', '/shares?tab=upload');
    const postPolicy = vi.spyOn(services.shares, 'postPolicy');
    const form = await screen.findByRole('form', { name: 'Upload link generator' });
    await user.type(within(form).getByRole('textbox', { name: /Bucket/ }), 'media');
    await user.type(within(form).getByRole('textbox', { name: /Key prefix/ }), 'incoming/');
    await user.click(within(form).getByRole('button', { name: 'Generate' }));
    const snippets = await screen.findByRole('region', { name: 'Snippets' });
    expect(postPolicy).toHaveBeenCalledWith(
      expect.objectContaining({
        bucket: 'media',
        keyPrefix: 'incoming/',
        maxSize: 10 * 1024 * 1024,
      }),
    );
    expect(snippets).toHaveTextContent('<form action="http://localhost:9000/media"');
    await user.click(within(snippets).getByRole('tab', { name: 'curl' }));
    expect(snippets).toHaveTextContent("--form-string 'key=incoming/${filename}'");
    await user.click(within(snippets).getByRole('button', { name: 'Copy the curl snippet' }));
    await waitFor(async () => {
      expect(await navigator.clipboard.readText()).toContain('curl -X POST');
    });
  });
});
