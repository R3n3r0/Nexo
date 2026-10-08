import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { ApiError } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { createUploadManager } from './manager';
import { UploadPanel } from './UploadPanel';
import { UploadProvider } from './UploadProvider';

const KiB = 1024;

function setup() {
  const services = createMockServices({
    initialAccessKey: 'nexoadmin',
    uploadLimits: { partSize: KiB, minPartSize: KiB, maxPartSize: 4 * KiB, maxParts: 100 },
  });
  // Parts hang until aborted, so that the file stays in flight.
  vi.spyOn(services.uploads, 'uploadPart').mockImplementation(
    (_input, options = {}) =>
      new Promise((_resolve, reject) => {
        options.signal?.addEventListener('abort', () => {
          reject(ApiError.aborted());
        });
      }),
  );
  const abort = vi.spyOn(services.uploads, 'abort');
  const manager = createUploadManager({ uploads: services.uploads, singleUploadThreshold: KiB });
  renderWithProviders(
    <UploadProvider manager={manager}>
      <UploadPanel />
    </UploadProvider>,
    { services },
  );
  return { manager, abort };
}

describe('UploadPanel (D-OBJ-02)', () => {
  it('is hidden while the queue is empty', () => {
    setup();
    expect(screen.queryByRole('region', { name: 'Uploads' })).not.toBeInTheDocument();
  });

  it('pauses, resumes and cancels a file, warning before leaving the page', async () => {
    const { manager, abort } = setup();
    act(() => {
      manager.enqueue('media', [
        { key: 'videos/big.bin', file: new Blob([new Uint8Array(3 * KiB)]) },
      ]);
    });
    const panel = await screen.findByRole('region', { name: 'Uploads' });
    expect(within(panel).getByText('1 active')).toBeInTheDocument();
    expect(
      within(panel).getByRole('progressbar', { name: 'Total upload progress' }),
    ).toBeInTheDocument();

    const leave = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(leave);
    expect(leave.defaultPrevented).toBe(true);

    const user = userEvent.setup();
    await user.click(await within(panel).findByRole('button', { name: 'Pause videos/big.bin' }));
    expect(await within(panel).findByText('Paused')).toBeInTheDocument();
    await user.click(within(panel).getByRole('button', { name: 'Resume videos/big.bin' }));
    expect(
      await within(panel).findByRole('button', { name: 'Pause videos/big.bin' }),
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(manager.getSnapshot().items[0]?.parts).toBe(3);
    });
    await user.click(within(panel).getByRole('button', { name: 'Cancel videos/big.bin' }));
    expect(await within(panel).findByText('Canceled')).toBeInTheDocument();
    await waitFor(() => {
      expect(abort).toHaveBeenCalledTimes(1);
    });

    const after = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(after);
    expect(after.defaultPrevented).toBe(false);

    await user.click(within(panel).getByRole('button', { name: 'Clear finished' }));
    expect(screen.queryByRole('region', { name: 'Uploads' })).not.toBeInTheDocument();
  });
});

describe('UploadPanel while completing (D-OBJ-02)', () => {
  // Regression: the completion could be "canceled" although the object was
  // created.
  it('disables the cancellation and tells why', async () => {
    const services = createMockServices({
      initialAccessKey: 'nexoadmin',
      uploadLimits: { partSize: KiB, minPartSize: KiB, maxPartSize: 4 * KiB, maxParts: 100 },
    });
    vi.spyOn(services.uploads, 'complete').mockImplementation(() => new Promise(() => undefined));
    const manager = createUploadManager({ uploads: services.uploads, singleUploadThreshold: KiB });
    renderWithProviders(
      <UploadProvider manager={manager}>
        <UploadPanel />
      </UploadProvider>,
      { services },
    );
    act(() => {
      manager.enqueue('media', [{ key: 'big.bin', file: new Blob([new Uint8Array(3 * KiB)]) }]);
    });
    const panel = await screen.findByRole('region', { name: 'Uploads' });
    expect(await within(panel).findByText('Completing')).toBeInTheDocument();
    const cancel = within(panel).getByRole('button', { name: 'Cancel big.bin' });
    expect(cancel).toBeDisabled();
    expect(cancel).toHaveAccessibleDescription(
      'Finishing the upload: it can no longer be canceled.',
    );
  });
});
