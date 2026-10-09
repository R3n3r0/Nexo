import type { Page } from '@playwright/test';

import { type NexoClient, uniqueName } from './support/api';
import { expect, test } from './support/fixtures';

/** A version as the Admin API lists it (newest first). */
interface Version {
  key: string;
  versionId?: string;
  isDeleteMarker: boolean;
}

const versionsOf = async (api: NexoClient, bucket: string, key: string) =>
  (
    await api.call<{ items: Version[] }>('GET', `/buckets/${bucket}/objects/versions`, {
      query: { key },
    })
  ).items;

/**
 * Lifts the Object Lock of every version (legal hold off, GOVERNANCE
 * retention removed with the bypass) so that the bucket can be deleted.
 */
async function unlockAll(api: NexoClient, bucket: string) {
  const keys = await api.listKeys(bucket).catch(() => [] as string[]);
  for (const key of keys) {
    for (const v of await versionsOf(api, bucket, key)) {
      if (v.isDeleteMarker) continue;
      const query = { key, versionId: v.versionId ?? 'null' };
      await api.admin('PUT', `/buckets/${bucket}/objects/legal-hold`, {
        query,
        json: { status: 'OFF' },
      });
      await api.admin('PUT', `/buckets/${bucket}/objects/retention`, {
        query,
        json: { bypassGovernance: true },
      });
    }
  }
}

/** Opens the drawer of an object of the current folder and one of its tabs. */
async function openDrawer(page: Page, name: string, tab: string) {
  await page.getByRole('button', { name, exact: true }).click();
  const drawer = page.getByRole('dialog', { name });
  await drawer.getByRole('tab', { name: tab }).click();
  return drawer;
}

test.describe('object lock in the console (D-OBJ-06)', () => {
  let bucket: string;

  test.beforeEach(() => {
    bucket = uniqueName('e2e-lock');
  });

  test.afterEach(async ({ api }) => {
    await unlockAll(api, bucket);
    await api.deleteBucket(bucket);
  });

  test('versioning, retention and legal hold in the drawer; object lock prevents the deletion with a clear message', async ({
    page,
    api,
  }) => {
    // Object lock at the creation of the bucket, after the warning.
    await page.goto('/buckets');
    await page.getByRole('button', { name: 'Create bucket' }).click();
    const create = page.getByRole('dialog', { name: 'Create a bucket' });
    await create.getByRole('textbox', { name: 'Name' }).fill(bucket);
    await create.getByText('Object lock', { exact: true }).click();
    await expect(create.getByText(/Object lock is irreversible/)).toBeVisible();
    await create.getByRole('button', { name: 'Create' }).click();
    await expect(page).toHaveURL(new RegExp(`/buckets/${bucket}/browse/$`));
    expect(
      (await api.call<{ enabled: boolean }>('GET', `/buckets/${bucket}/objectlock`)).enabled,
    ).toBe(true);

    // Two versions and a delete marker between them.
    await api.putObject(bucket, 'doc.txt', 'first\n', 'text/plain');
    await api.s3('DELETE', `/${bucket}/doc.txt`);
    await api.putObject(bucket, 'doc.txt', 'second\n', 'text/plain');
    await page.reload();

    // Retention: GOVERNANCE for a week.
    const drawer = await openDrawer(page, 'doc.txt', 'Object lock');
    const retention = drawer.getByRole('region', { name: 'Retention' });
    await expect(retention.getByText('None', { exact: true })).toBeVisible();
    await retention.getByRole('textbox', { name: 'Mode' }).click();
    await page.getByRole('option', { name: 'GOVERNANCE' }).click();
    await retention.getByRole('button', { name: '+7 days' }).click();
    await retention.getByRole('button', { name: 'Save retention' }).click();
    await expect(page.getByText('Retention saved.')).toBeVisible();
    await expect(retention.getByText(/^GOVERNANCE until/)).toBeVisible();
    const [latest] = await versionsOf(api, bucket, 'doc.txt');
    const versionId = latest?.versionId ?? '';
    const stored = await api.call<{ mode?: string; retainUntil?: string }>(
      'GET',
      `/buckets/${bucket}/objects/retention`,
      { query: { key: 'doc.txt', versionId } },
    );
    expect(stored.mode).toBe('GOVERNANCE');
    expect(Date.parse(stored.retainUntil ?? '')).toBeGreaterThan(Date.now() + 6 * 86_400_000);

    // Legal hold on.
    const hold = drawer.getByRole('region', { name: 'Legal hold' });
    await hold.getByText('Legal hold active').click();
    await expect(page.getByText('Legal hold set.')).toBeVisible();
    expect(
      (
        await api.call<{ status: string }>('GET', `/buckets/${bucket}/objects/legal-hold`, {
          query: { key: 'doc.txt', versionId },
        })
      ).status,
    ).toBe('ON');

    // The versions, the delete marker highlighted.
    await drawer.getByRole('tab', { name: 'Versions' }).click();
    const table = drawer.getByRole('table', { name: 'Versions' });
    await expect(table.getByRole('row')).toHaveCount(3);
    await expect(table.locator('tr[data-delete-marker]')).toHaveCount(1);
    await expect(table.locator('tr[data-delete-marker]').getByText('Delete marker')).toBeVisible();

    // The legal hold prevents the deletion, with the reason.
    await table.getByRole('button', { name: `Delete version ${versionId}` }).click();
    let dialog = page.getByRole('dialog', { name: `Delete version ${versionId}` });
    await expect(
      dialog.getByText(
        'This version is under legal hold: it cannot be deleted until the hold is removed.',
      ),
    ).toBeVisible();
    await expect(dialog.getByRole('button', { name: 'Delete permanently' })).toBeDisabled();
    await dialog.getByRole('button', { name: 'Cancel' }).click();

    // Without the hold the GOVERNANCE retention still protects it, unless bypassed.
    await drawer.getByRole('tab', { name: 'Object lock' }).click();
    await hold.getByText('Legal hold active').click();
    await expect(page.getByText('Legal hold removed.')).toBeVisible();
    await drawer.getByRole('tab', { name: 'Versions' }).click();
    await table.getByRole('button', { name: `Delete version ${versionId}` }).click();
    dialog = page.getByRole('dialog', { name: `Delete version ${versionId}` });
    await expect(dialog.getByText(/protected by a GOVERNANCE retention until/)).toBeVisible();
    const remove = dialog.getByRole('button', { name: 'Delete permanently' });
    await expect(remove).toBeDisabled();
    await dialog.getByText('Bypass the governance retention').click();
    await expect(remove).toBeEnabled();
    await remove.click();
    await expect(page.getByText('Version deleted.')).toBeVisible();
    expect((await versionsOf(api, bucket, 'doc.txt')).map((v) => v.versionId)).not.toContain(
      versionId,
    );
  });

  test('keeps the protected versions of a folder deleted with every version', async ({
    page,
    api,
  }) => {
    await api.call('POST', '/buckets', { json: { name: bucket, objectLock: true } });
    await api.putObject(bucket, 'docs/a.txt', 'a\n', 'text/plain');
    const [version] = await versionsOf(api, bucket, 'docs/a.txt');
    await api.call('PUT', `/buckets/${bucket}/objects/legal-hold`, {
      query: { key: 'docs/a.txt', versionId: version?.versionId ?? '' },
      json: { status: 'ON' },
    });

    await page.goto(`/buckets/${bucket}/browse/`);
    await page.getByRole('checkbox', { name: 'Select docs/' }).check();
    await page.getByRole('button', { name: 'Delete (1)' }).click();
    const dialog = page.getByRole('dialog', { name: 'Delete 1 items' });
    await dialog.getByText('Delete every version permanently').click();
    await expect(
      dialog.getByText('The versions under retention or legal hold are kept.'),
    ).toBeVisible();
    await dialog.getByRole('textbox').fill('docs/');
    await dialog.getByRole('button', { name: 'Delete' }).click();
    await expect(page.getByText('0 deleted; 1 protected by object lock were kept.')).toBeVisible();
    expect(await api.listKeys(bucket, 'docs/')).toEqual(['docs/a.txt']);
  });
});
