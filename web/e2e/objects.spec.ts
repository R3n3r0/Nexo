import { mkdtemp, readFile } from 'node:fs/promises';
import { join } from 'node:path';

import type { Page } from '@playwright/test';

import { uniqueName } from './support/api';
import { dropFromDisk, writeNestedFolder } from './support/files';
import { expect, test } from './support/fixtures';
import { readZip } from './support/zip';

/** The row of an object (a button opening its details) of the browser. */
const row = (page: Page, name: string) =>
  page.getByRole('row').filter({ has: page.getByRole('button', { name, exact: true }) });

/** The row of a folder (a link to its listing) of the browser: name ends with "/". */
const folderRow = (page: Page, name: string) =>
  page.getByRole('row').filter({ has: page.getByRole('link', { name, exact: true }) });

test.describe('object browser', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-objects');
    await api.createBucket(bucket);
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  // covers: D-OBJ-02
  test('TD-E-05 dropping a folder of 50 nested files keeps its structure', async ({
    page,
    api,
    nexo,
  }) => {
    const dir = await mkdtemp(join(nexo.workDir, 'drop-'));
    const paths = await writeNestedFolder(dir, 'album', 50);
    await api.call('POST', `/buckets/${bucket}/objects/folder`, { json: { prefix: 'inbox/' } });

    await page.goto(`/buckets/${bucket}/browse/inbox/`);
    await expect(page.getByText('This folder is empty.')).toBeVisible();
    await dropFromDisk(page, '[data-testid="object-dropzone"]', [join(dir, 'album')]);

    const uploads = page.getByRole('region', { name: 'Uploads' });
    await expect(uploads.getByText('Completed')).toHaveCount(50, { timeout: 30_000 });
    await expect(folderRow(page, 'album/')).toBeVisible();
    const expected = paths.map((p) => `inbox/${p}`);
    expect((await api.listKeys(bucket, 'inbox/')).filter((k) => k !== 'inbox/')).toEqual(expected);

    // The structure is browsable: open the nested folders down to a file.
    await page.getByRole('link', { name: 'album/', exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/browse/inbox/album/$`));
    await page.getByRole('link', { name: 'l1-1/', exact: true }).click();
    await expect(page.getByRole('navigation', { name: 'Folder path' })).toContainText('l1-1');
  });

  // covers: D-OBJ-03
  test('TD-E-07 downloads one object and a zip of three', async ({ page, api }) => {
    const files = {
      'reports/a.txt': 'alpha\n',
      'reports/b.txt': 'bravo\n',
      'reports/c.csv': 'x,y\n1,2\n',
    };
    for (const [key, body] of Object.entries(files)) {
      await api.putObject(bucket, key, body, 'text/plain');
    }
    await page.goto(`/buckets/${bucket}/browse/reports/`);

    // Single object: the browser downloads the original bytes.
    await row(page, 'a.txt').getByRole('button', { name: 'Actions for a.txt' }).click();
    const single = page.waitForEvent('download');
    await page.getByRole('menuitem', { name: 'Download', exact: true }).click();
    const download = await single;
    expect(download.suggestedFilename()).toBe('a.txt');
    expect(await readFile(await download.path(), 'utf8')).toBe(files['reports/a.txt']);

    // Three objects: one zip archive with the three files.
    for (const name of ['a.txt', 'b.txt', 'c.csv']) {
      await page.getByRole('checkbox', { name: `Select ${name}` }).check();
    }
    const zipped = page.waitForEvent('download');
    await page.getByRole('button', { name: 'Download (3)' }).click();
    const archive = await zipped;
    expect(archive.suggestedFilename()).toMatch(/\.zip$/);
    const entries = readZip(await readFile(await archive.path()));
    const contents = Object.fromEntries(
      [...entries].map(([name, data]) => [name, data.toString('utf8')]),
    );
    expect(Object.values(contents).sort()).toEqual(Object.values(files).sort());
    expect(Object.keys(contents).map((n) => n.split('/').pop())).toEqual(
      expect.arrayContaining(['a.txt', 'b.txt', 'c.csv']),
    );
  });

  // covers: D-OBJ-03
  test('TD-E-07 a refused zip download reports its problem and downloads nothing', async ({
    page,
    api,
  }) => {
    await api.putObject(bucket, 'gone.txt', 'soon deleted', 'text/plain');
    await api.putObject(bucket, 'kept.txt', 'kept', 'text/plain');
    await page.goto(`/buckets/${bucket}/browse/`);
    for (const name of ['gone.txt', 'kept.txt']) {
      await page.getByRole('checkbox', { name: `Select ${name}` }).check();
    }
    // The archive names an object that no longer exists: the server
    // answers 404 before streaming, in the hidden frame of the form.
    await api.call('DELETE', `/buckets/${bucket}/objects`, {
      json: { objects: [{ key: 'gone.txt' }] },
    });
    let downloads = 0;
    page.on('download', () => {
      downloads += 1;
    });
    await page.getByRole('button', { name: 'Download (2)' }).click();
    const alert = page.getByRole('alert').filter({ hasText: 'The archive could not be created' });
    await expect(alert).toBeVisible();
    await expect(alert).toContainText('The object does not exist.');
    expect(downloads).toBe(0);
  });

  // covers: D-OBJ-04
  test('TD-E-08 creates a folder, moves an object into it and deletes the folder', async ({
    page,
    api,
  }) => {
    await api.putObject(bucket, 'notes.txt', 'remember the milk', 'text/plain');
    await page.goto(`/buckets/${bucket}/browse/`);

    await page.getByRole('button', { name: 'New folder' }).click();
    const create = page.getByRole('dialog', { name: 'New folder' });
    await create.getByRole('textbox', { name: 'Folder name' }).fill('archive/2026');
    await create.getByRole('button', { name: 'Create' }).click();
    await expect(folderRow(page, 'archive/')).toBeVisible();

    await row(page, 'notes.txt').getByRole('button', { name: 'Actions for notes.txt' }).click();
    await page.getByRole('menuitem', { name: 'Rename or move' }).click();
    const move = page.getByRole('dialog', { name: 'Move notes.txt' });
    await move.getByRole('textbox', { name: 'New key' }).fill('archive/2026/notes.txt');
    await move.getByRole('button', { name: 'Move' }).click();
    await expect(move).toBeHidden();
    await expect(row(page, 'notes.txt')).toBeHidden();
    await expect.poll(() => api.listKeys(bucket)).toContain('archive/2026/notes.txt');
    expect(await api.listKeys(bucket)).not.toContain('notes.txt');

    // Recursive deletion of the folder, confirmed by typing its path.
    await page.getByRole('checkbox', { name: 'Select archive/' }).check();
    await page.getByRole('button', { name: 'Delete (1)' }).click();
    const remove = page.getByRole('dialog', { name: 'Delete 1 items' });
    await expect(remove).toContainText('archive/ (folder, recursive)');
    await remove.getByRole('textbox').fill('archive/');
    await remove.getByRole('button', { name: 'Delete' }).click();
    await expect(remove).toBeHidden();
    await expect(page.getByText('This folder is empty.')).toBeVisible();
    await expect.poll(() => api.listKeys(bucket)).toEqual([]);
  });
});
