import { uniqueName } from './support/api';
import { e2eKmsKeyName } from './support/env';
import { expect, test } from './support/fixtures';

test.describe('server-side encryption and CORS in the console', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-sse');
    await api.createBucket(bucket);
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  test('shows the KMS and creates a key', async ({ page }) => {
    await page.goto('/info');
    const kms = page.getByRole('region', { name: 'Key management (KMS)' });
    await expect(kms.getByText('Builtin (master key from the environment)')).toBeVisible();
    await expect(kms.getByText(e2eKmsKeyName, { exact: true })).toBeVisible();

    const name = uniqueName('e2e-key');
    await page.goto('/configuration');
    const section = page.getByRole('region', { name: 'Key management (KMS)' });
    await section.getByRole('textbox', { name: 'New key' }).fill(name);
    await section.getByRole('button', { name: 'Create key' }).click();
    await expect(page.getByText(`Key ${name} created.`)).toBeVisible();
    await expect(section.getByRole('table', { name: 'Keys' }).getByText(name)).toBeVisible();
  });

  test('uploads an object with SSE-S3 and shows its encryption', async ({ page, api }) => {
    await page.goto(`/buckets/${bucket}/browse/`);
    await page.getByRole('button', { name: 'Upload', exact: true }).click();
    await page.getByRole('menuitem', { name: 'SSE-S3 (AES-256)' }).click();
    await page.keyboard.press('Escape');
    await expect(page.getByLabel('Uploads encrypted with SSE-S3')).toBeVisible();
    await page.getByTestId('upload-files-input').setInputFiles({
      name: 'secret.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from('top secret\n'),
    });
    await expect(
      page.getByRole('region', { name: 'Uploads' }).getByText('Completed'),
    ).toBeVisible();

    const head = await api.s3('HEAD', `/${bucket}/secret.txt`);
    expect(head.headers.get('x-amz-server-side-encryption')).toBe('AES256');
    await page.getByRole('button', { name: 'secret.txt', exact: true }).click();
    const drawer = page.getByRole('dialog', { name: 'secret.txt' });
    await expect(drawer.getByRole('row', { name: /Encryption/ }).getByText('SSE-S3')).toBeVisible();
    // The content reads back in clear through the console.
    const res = await api.s3('GET', `/${bucket}/secret.txt`);
    expect(await res.text()).toBe('top secret\n');
  });

  test('sets the default encryption to SSE-KMS and CORS rules in the settings', async ({
    page,
    api,
  }) => {
    await page.goto(`/buckets/${bucket}/settings`);
    const encryption = page.getByRole('region', { name: 'Encryption' });
    await encryption.getByRole('textbox', { name: 'Default encryption' }).click();
    await page.getByRole('option', { name: 'SSE-KMS', exact: true }).click();
    await encryption.getByRole('textbox', { name: 'KMS key ID' }).fill(e2eKmsKeyName);
    await encryption.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText('Settings saved.')).toBeVisible();
    expect(await api.call('GET', `/buckets/${bucket}/encryption`)).toEqual({
      algorithm: 'aws:kms',
      kmsKeyId: e2eKmsKeyName,
    });
    await api.putObject(bucket, 'auto.txt', 'sealed by default\n', 'text/plain');
    const head = await api.s3('HEAD', `/${bucket}/auto.txt`);
    expect(head.headers.get('x-amz-server-side-encryption')).toBe('aws:kms');

    const cors = page.getByRole('region', { name: 'CORS' });
    await expect(
      cors.getByText('No CORS rule: the browsers of other origins are refused.'),
    ).toBeVisible();
    await cors.getByRole('button', { name: 'Add rule' }).click();
    const rule = cors.getByRole('group', { name: 'Rule 1' });
    await rule.getByRole('textbox', { name: 'Allowed origins' }).fill('https://app.example.com');
    await rule.getByRole('checkbox', { name: 'PUT' }).check();
    await rule.getByRole('textbox', { name: 'Max age (seconds)' }).fill('600');
    await cors.getByRole('button', { name: 'Save CORS rules' }).click();
    await expect(page.getByText('CORS rules saved.')).toBeVisible();
    const s3 = await api.s3('GET', `/${bucket}`, { query: { cors: '' } });
    expect(s3.status).toBe(200);
    const doc = await s3.text();
    expect(doc).toContain('<AllowedOrigin>https://app.example.com</AllowedOrigin>');
    expect(doc).toContain('<AllowedMethod>PUT</AllowedMethod>');
    expect(doc).toContain('<MaxAgeSeconds>600</MaxAgeSeconds>');
  });
});
