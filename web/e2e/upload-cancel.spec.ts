import { open, mkdtemp } from 'node:fs/promises';
import { join } from 'node:path';

import type { Route } from '@playwright/test';

import { uniqueName } from './support/api';
import { expect, test } from './support/fixtures';

const MiB = 1024 * 1024;

test('TD-E-06 canceling a 200 MiB upload halfway leaves no pending multipart upload', async ({
  page,
  api,
  nexo,
}) => {
  test.setTimeout(180_000);
  const bucket = uniqueName('e2e-cancel');
  await api.createBucket(bucket);
  try {
    // A sparse 200 MiB file: the browser reads it from the disk.
    const dir = await mkdtemp(join(nexo.workDir, 'big-'));
    const file = join(dir, 'big.bin');
    const handle = await open(file, 'w');
    await handle.truncate(200 * MiB);
    await handle.close();

    // The first part goes through; the next ones are held, as on a slow
    // link, so that the upload is canceled in the middle. One part only:
    // the test does not depend on the throughput of a loaded machine.
    const passed: number[] = [];
    const held: Route[] = [];
    await page.route('**/api/v1/buckets/*/uploads/*/parts/*', async (route) => {
      if (passed.length < 1) {
        passed.push(Number(new URL(route.request().url()).pathname.split('/').pop()));
        await route.continue();
        return;
      }
      held.push(route);
    });

    await page.goto(`/buckets/${bucket}/browse/`);
    await page.getByTestId('upload-files-input').setInputFiles(file);
    const uploads = page.getByRole('region', { name: 'Uploads' });
    await expect(uploads.getByText('big.bin')).toBeVisible();
    // Generous timeouts: a part is 16 MiB read from the disk by the
    // browser and sent to the server, slow on a busy CI runner.
    await expect.poll(() => passed.length, { timeout: 60_000 }).toBe(1);
    await expect.poll(() => held.length, { timeout: 60_000 }).toBeGreaterThan(0);

    // The multipart upload exists on the server while the file is in flight.
    const pending = await api.call<{ items: { key: string; uploadId: string }[] }>(
      'GET',
      `/buckets/${bucket}/uploads`,
    );
    expect(pending.items.map((u) => u.key)).toEqual(['big.bin']);

    await uploads.getByRole('button', { name: 'Cancel big.bin' }).click();
    await expect(uploads.getByText('Canceled')).toBeVisible();
    for (const route of held) {
      await route.abort().catch(() => undefined);
    }

    // Nothing pending on the server, through the Admin API and S3.
    await expect
      .poll(
        async () =>
          (await api.call<{ items: unknown[] }>('GET', `/buckets/${bucket}/uploads`)).items.length,
        { timeout: 15_000 },
      )
      .toBe(0);
    const s3 = await api.s3('GET', `/${bucket}`, { query: { uploads: '' } });
    expect(s3.status).toBe(200);
    expect(await s3.text()).not.toContain('<Upload>');
    expect(await api.listKeys(bucket)).toEqual([]);
  } finally {
    await api.deleteBucket(bucket);
  }
});
