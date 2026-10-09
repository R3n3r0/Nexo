import { uniqueName } from './support/api';
import { Nexoctl } from './support/cli';
import { editorLines, setEditorText } from './support/editor';
import { expect, test } from './support/fixtures';

/** A rule of the Admin API (GET /buckets/{bucket}/lifecycle). */
interface Rule {
  id?: string;
  enabled?: boolean;
  filter?: { prefix?: string };
  expiration?: { days?: number };
  noncurrentExpiration?: { noncurrentDays?: number; newerNoncurrentVersions?: number };
  abortIncompleteUpload?: { daysAfterInitiation?: number };
}

test.describe('bucket lifecycle (D-BKT-03)', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-ilm');
    await api.createBucket(bucket, { versioning: true });
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  test('TD-E-14 a rule created in the console is the one of the S3 API and of the CLI', async ({
    page,
    api,
    nexo,
  }) => {
    await page.goto(`/buckets/${bucket}/lifecycle`);
    const section = page.getByRole('region', { name: 'Lifecycle rules' });
    await expect(
      section.getByText('No lifecycle rule: the objects are kept until they are deleted.'),
    ).toBeVisible();

    // The guided form: prefix, expiration, noncurrent versions, uploads.
    await section.getByRole('button', { name: 'Add rule' }).click();
    const editor = page.getByRole('dialog', { name: 'New lifecycle rule' });
    await editor.getByRole('textbox', { name: 'Rule ID' }).fill('expire-logs');
    await editor.getByRole('textbox', { name: 'Prefix' }).fill('logs/');
    await editor.getByRole('textbox', { name: 'Days after the creation' }).fill('45');
    await editor.getByText('Expire the noncurrent versions').click();
    await editor.getByRole('textbox', { name: 'Days after becoming noncurrent' }).fill('7');
    await editor.getByRole('textbox', { name: 'Newer versions to keep' }).fill('2');
    await editor.getByText('Abort the incomplete uploads').click();

    // The XML view follows the form.
    await editor.getByText('XML', { exact: true }).click();
    await expect(editorLines(editor)).toContainText('<Prefix>logs/</Prefix>');
    await expect(editorLines(editor)).toContainText(
      '<NewerNoncurrentVersions>2</NewerNoncurrentVersions>',
    );
    await editor.getByText('Form', { exact: true }).click();
    await editor.getByRole('button', { name: 'Save rule' }).click();
    await expect(page.getByText('Lifecycle rules saved.')).toBeVisible();

    const table = section.getByRole('table', { name: 'Lifecycle rules' });
    await expect(table.getByText('expire-logs')).toBeVisible();
    await expect(table.getByText('Current versions expire after 45 days')).toBeVisible();
    await expect(
      table.getByText('Noncurrent versions expire after 7 days, the 2 newest are kept'),
    ).toBeVisible();
    await expect(table.getByText('Incomplete uploads are aborted after 7 days')).toBeVisible();

    // The Admin API and the S3 API store the same document.
    const stored = await api.call<{ rules: Rule[] }>('GET', `/buckets/${bucket}/lifecycle`);
    expect(stored.rules).toEqual([
      {
        id: 'expire-logs',
        enabled: true,
        filter: { prefix: 'logs/' },
        expiration: { days: 45 },
        noncurrentExpiration: { noncurrentDays: 7, newerNoncurrentVersions: 2 },
        abortIncompleteUpload: { daysAfterInitiation: 7 },
      },
    ]);
    const s3 = await api.s3('GET', `/${bucket}`, { query: { lifecycle: '' } });
    expect(s3.status).toBe(200);
    const doc = await s3.text();
    expect(doc).toContain('<ID>expire-logs</ID>');
    expect(doc).toContain('<Days>45</Days>');
    expect(doc).toContain('<DaysAfterInitiation>7</DaysAfterInitiation>');

    // The CLI lists it.
    const cli = new Nexoctl(nexo);
    const listed = await cli.ok('ilm', 'rule', 'ls', '--json', `e2e/${bucket}`);
    expect(listed).toContain('"id":"expire-logs"');
    expect(listed).toContain('"prefix":"logs/"');

    // A rule the server refuses names the problem of the rule.
    await section.getByRole('button', { name: 'Add rule' }).click();
    await editor.getByRole('textbox', { name: 'Rule ID' }).fill('expire-logs');
    await editor.getByRole('button', { name: 'Save rule' }).click();
    await expect(editor.getByText('Another rule has this ID.')).toBeVisible();
    await editor.getByRole('button', { name: 'Cancel' }).click();

    // Deleting the last rule removes the configuration.
    await table.getByRole('button', { name: 'Delete rule expire-logs' }).click();
    await page
      .getByRole('dialog', { name: 'Delete the rule' })
      .getByRole('button', { name: 'Delete' })
      .click();
    await expect(
      section.getByText('No lifecycle rule: the objects are kept until they are deleted.'),
    ).toBeVisible();
    expect(
      (await api.call<{ rules: Rule[] }>('GET', `/buckets/${bucket}/lifecycle`)).rules,
    ).toEqual([]);
  });

  test('saves a configuration written as S3 XML and applies it now', async ({ page, api }) => {
    await api.putObject(bucket, 'tmp/old.txt', 'old\n', 'text/plain');
    await page.goto(`/buckets/${bucket}/lifecycle`);
    const section = page.getByRole('region', { name: 'Lifecycle rules' });
    await section.getByText('XML', { exact: true }).click();
    await setEditorText(
      page,
      section,
      'LifecycleConfiguration (S3 XML)',
      '<LifecycleConfiguration><Rule><ID>tmp</ID><Filter><Prefix>tmp/</Prefix></Filter>' +
        '<Status>Enabled</Status><Expiration><Date>2020-01-01T00:00:00Z</Date></Expiration>' +
        '</Rule></LifecycleConfiguration>',
    );
    await section.getByRole('button', { name: 'Save the configuration' }).click();
    await expect(page.getByText('Lifecycle rules saved.')).toBeVisible();
    await section.getByText('Rules', { exact: true }).click();
    await expect(section.getByText('Prefix tmp/')).toBeVisible();

    // The date is past: applying now expires the object (a delete marker).
    await section.getByRole('button', { name: 'Apply now' }).click();
    await page
      .getByRole('dialog', { name: 'Apply the lifecycle rules now' })
      .getByRole('button', { name: 'Apply now' })
      .click();
    await expect(page.getByText('Lifecycle rules applied')).toBeVisible();
    const res = await api.s3('GET', `/${bucket}/tmp/old.txt`);
    expect(res.status).toBe(404);
  });
});
