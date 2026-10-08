import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { ShareDialog, type ShareDialogProps } from './ShareDialog';

function renderDialog(props: Partial<ShareDialogProps> = {}) {
  const services = createMockServices({ initialAccessKey: 'nexoadmin' });
  const onClose = vi.fn();
  const view = renderRoutes(
    [
      {
        path: '/',
        element: (
          <ShareDialog
            opened
            onClose={onClose}
            bucket="media"
            objectKey="docs/report.pdf"
            {...props}
          />
        ),
      },
    ],
    { services },
  );
  return { ...view, services, onClose };
}

describe('ShareDialog (TD-U-10)', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('creates a managed link valid at most 7 days with the 7 days preset', async () => {
    const user = userEvent.setup();
    const { services } = renderDialog();
    const create = vi.spyOn(services.shares, 'create');
    await user.click(await screen.findByRole('radio', { name: '7 days' }));
    await user.click(screen.getByRole('button', { name: 'Create link' }));
    await screen.findByText(/Share link created/);
    expect(create).toHaveBeenCalledWith(
      expect.objectContaining({
        bucket: 'media',
        key: 'docs/report.pdf',
        prefix: false,
        permission: 'download',
        expiresIn: 604_800,
      }),
    );
    expect(screen.getByRole('img', { name: 'QR code of the link' })).toBeInTheDocument();
  });

  it('refuses a custom expiration beyond 7 days', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-10-08T12:00:00Z'));
    const user = userEvent.setup();
    renderDialog();
    await user.click(await screen.findByRole('radio', { name: 'Until…' }));
    // Without a date the link cannot be created.
    expect(screen.getByRole('button', { name: 'Create link' })).toBeDisabled();
    expect(screen.getByText('Pick the expiration date.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /Expiration date/ }));
    // Days within 7 days are selectable, later ones are not.
    expect(screen.getByRole('button', { name: '15 October 2026' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '16 October 2026' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '20 October 2026' })).toBeDisabled();
  });

  it('copies the link to the clipboard', async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.click(await screen.findByRole('button', { name: 'Create link' }));
    const link = await screen.findByRole('textbox', { name: 'Link' });
    const url = (link as HTMLInputElement).value;
    expect(url).toMatch(/^http:\/\/localhost:9000\/media\/docs\/report\.pdf\?/);
    await user.click(screen.getByRole('button', { name: 'Copy link' }));
    await waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe(url);
    });
  });

  it('sends the content disposition of downloads', async () => {
    const user = userEvent.setup();
    const { services } = renderDialog();
    const create = vi.spyOn(services.shares, 'create');
    await user.click(await screen.findByRole('textbox', { name: /When opened/ }));
    await user.click(await screen.findByRole('option', { name: 'Download as a file' }));
    await user.click(screen.getByRole('button', { name: 'Create link' }));
    await screen.findByText(/Share link created/);
    expect(create).toHaveBeenCalledWith(
      expect.objectContaining({ contentDisposition: 'attachment; filename="report.pdf"' }),
    );
  });

  it('warns that a raw presigned link cannot be revoked and signs it with /presign', async () => {
    const user = userEvent.setup();
    const { services } = renderDialog({ versionId: 'v1' });
    const presign = vi.spyOn(services.shares, 'presign');
    const create = vi.spyOn(services.shares, 'create');
    await user.click(await screen.findByRole('radio', { name: 'Upload' }));
    await user.click(screen.getByRole('switch', { name: /Raw presigned link/ }));
    expect(screen.getByRole('alert')).toHaveTextContent(/cannot be revoked/);
    await user.click(screen.getByRole('button', { name: 'Create link' }));
    await screen.findByText(/It cannot be revoked/);
    expect(presign).toHaveBeenCalledWith(
      expect.objectContaining({
        method: 'PUT',
        bucket: 'media',
        key: 'docs/report.pdf',
        versionId: 'v1',
      }),
    );
    expect(create).not.toHaveBeenCalled();
    expect(screen.getByText(/--upload-file/)).toBeInTheDocument();
  });

  it('shares a prefix for download only', async () => {
    const user = userEvent.setup();
    const { services } = renderDialog({ objectKey: undefined, prefix: 'docs/' });
    const create = vi.spyOn(services.shares, 'create');
    expect(await screen.findByRole('radio', { name: 'Upload' })).toBeDisabled();
    expect(screen.getByRole('switch', { name: /Raw presigned link/ })).toBeDisabled();
    const form = screen.getByRole('form', { name: 'Share settings' });
    await user.click(within(form).getByRole('button', { name: 'Create link' }));
    await screen.findByText(/Share link created/);
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ key: 'docs/', prefix: true }));
  });
});
