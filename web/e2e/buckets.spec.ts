import { uniqueName } from './support/api';
import { expect, test } from './support/fixtures';

test('TD-E-04 creates a bucket, finds it in the list and deletes it', async ({ page, api }) => {
  const bucket = uniqueName('e2e-bucket');
  try {
    await page.goto('/buckets');
    await page.getByRole('button', { name: 'Create bucket' }).click();
    const create = page.getByRole('dialog', { name: 'Create a bucket' });
    const name = create.getByRole('textbox', { name: 'Name' });
    // The DNS rules are checked while typing.
    await name.fill('Bad_Name');
    await expect(
      create.getByText('Use only lowercase letters, digits, dots and hyphens.'),
    ).toBeVisible();
    await name.fill(bucket);
    await create.getByText('Versioning', { exact: true }).click();
    await expect(create.getByRole('switch', { name: /^Versioning/ })).toBeChecked();
    await create.getByRole('button', { name: 'Create' }).click();
    await expect(create).toBeHidden();
    await expect(page.getByText(`Bucket ${bucket} created.`)).toBeVisible();
    // The new bucket opens on its (empty) object browser.
    await expect(page).toHaveURL(new RegExp(`/buckets/${bucket}/browse/$`));
    await expect(page.getByRole('heading', { name: bucket })).toBeVisible();
    await page.getByRole('main').getByRole('link', { name: 'Buckets', exact: true }).click();

    const table = page.getByRole('table', { name: 'Buckets' });
    await page.getByRole('textbox', { name: 'Search buckets' }).fill(bucket);
    await expect(table.getByRole('link', { name: bucket, exact: true })).toBeVisible();
    const info = await api.call<{ name: string; versioning: string }>('GET', `/buckets/${bucket}`);
    expect(info).toMatchObject({ name: bucket, versioning: 'enabled' });

    // A duplicate is refused with a clear message.
    await page.getByRole('button', { name: 'Create bucket' }).click();
    await create.getByRole('textbox', { name: 'Name' }).fill(bucket);
    await create.getByRole('button', { name: 'Create' }).click();
    await expect(create.getByText('A bucket with this name already exists.')).toBeVisible();
    await create.getByRole('button', { name: 'Cancel' }).click();

    await page.getByRole('button', { name: `Actions for ${bucket}` }).click();
    await page.getByRole('menuitem', { name: 'Delete' }).click();
    const remove = page.getByRole('dialog', { name: `Delete bucket ${bucket}` });
    const confirm = remove.getByRole('button', { name: 'Delete' });
    await expect(confirm).toBeDisabled();
    await remove.getByRole('textbox', { name: `Type ${bucket} to confirm` }).fill(bucket);
    await confirm.click();
    await expect(remove).toBeHidden();
    await expect(page.getByText(`Bucket ${bucket} deleted.`)).toBeVisible();
    await expect(table.getByRole('link', { name: bucket, exact: true })).toBeHidden();
    const gone = await api.admin('GET', `/buckets/${bucket}`);
    expect(gone.status).toBe(404);
  } finally {
    await api.deleteBucket(bucket);
  }
});
