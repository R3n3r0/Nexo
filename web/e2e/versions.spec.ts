import { uniqueName } from './support/api';
import { expect, test } from './support/fixtures';

/** A version of an object as the Admin API lists it (newest first). */
interface Version {
  key: string;
  versionId?: string;
  size: number;
  isLatest: boolean;
  isDeleteMarker: boolean;
}

test.describe('object versions', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-versions');
    await api.createBucket(bucket);
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  test('TD-E-11 enables versioning, uploads 3 versions, restores the first and deletes one', async ({
    page,
    api,
  }) => {
    const versionsOf = async (key: string): Promise<Version[]> =>
      (
        await api.call<{ items: Version[] }>('GET', `/buckets/${bucket}/objects/versions`, {
          query: { key },
        })
      ).items;
    const content = async (key: string): Promise<string> => {
      const res = await api.s3('GET', `/${bucket}/${key}`);
      expect(res.status).toBe(200);
      return res.text();
    };

    // Versioning from the settings tab.
    await page.goto(`/buckets/${bucket}/settings`);
    const versioning = page.getByRole('region', { name: 'Versioning' });
    await expect(versioning.getByText('Never enabled')).toBeVisible();
    await versioning.getByText('Enabled', { exact: true }).click();
    await expect(page.getByText('Settings saved.')).toBeVisible();
    expect(
      (await api.call<{ status: string }>('GET', `/buckets/${bucket}/versioning`)).status,
    ).toBe('enabled');

    // Three uploads of the same key from the console: three versions.
    await page.goto(`/buckets/${bucket}/browse/`);
    const uploads = page.getByRole('region', { name: 'Uploads' });
    const bodies = ['first version\n', 'second version, longer\n', 'third\n'];
    for (const [i, body] of bodies.entries()) {
      await page.getByTestId('upload-files-input').setInputFiles({
        name: 'doc.txt',
        mimeType: 'text/plain',
        buffer: Buffer.from(body),
      });
      await expect(uploads.getByText('Completed')).toHaveCount(i + 1);
    }
    const uploaded = await versionsOf('doc.txt');
    expect(uploaded.map((v) => v.size)).toEqual(bodies.map((b) => b.length).reverse());
    const [third, second, first] = uploaded.map((v) => v.versionId ?? 'null');
    expect(await content('doc.txt')).toBe(bodies[2]);

    // The drawer lists them, newest first.
    await page.getByRole('button', { name: 'doc.txt', exact: true }).click();
    const drawer = page.getByRole('dialog', { name: 'doc.txt' });
    await drawer.getByRole('tab', { name: 'Versions' }).click();
    const table = drawer.getByRole('table', { name: 'Versions' });
    await expect(table.getByRole('row')).toHaveCount(3);
    // The latest version has nothing to restore.
    await expect(table.getByRole('button', { name: `Restore version ${third}` })).toHaveCount(0);

    // Restoring the first version makes its content the latest again and
    // keeps the history.
    await table.getByRole('button', { name: `Restore version ${first}` }).click();
    await expect(
      page.getByText(`Version ${first} restored: it is the latest again.`),
    ).toBeVisible();
    await expect(table.getByRole('row')).toHaveCount(4);
    expect(await content('doc.txt')).toBe(bodies[0]);
    const restored = await versionsOf('doc.txt');
    expect(restored).toHaveLength(4);
    expect(restored[0]?.isLatest).toBe(true);
    expect(restored[0]?.size).toBe(bodies[0]?.length);
    expect(restored.slice(1).map((v) => v.versionId)).toEqual([third, second, first]);

    // Deleting the second version removes it for good, after a confirmation.
    await table.getByRole('button', { name: `Delete version ${second}` }).click();
    await table.getByRole('button', { name: `Confirm the deletion of version ${second}` }).click();
    await expect(page.getByText('Version deleted.')).toBeVisible();
    await expect(table.getByRole('row')).toHaveCount(3);
    const remaining = await versionsOf('doc.txt');
    expect(remaining.map((v) => v.versionId)).toEqual([restored[0]?.versionId, third, first]);
    expect(remaining.some((v) => v.isDeleteMarker)).toBe(false);
    expect(await content('doc.txt')).toBe(bodies[0]);
    const old = await api.s3('GET', `/${bucket}/doc.txt`, { query: { versionId: first } });
    expect(await old.text()).toBe(bodies[0]);
  });
});
