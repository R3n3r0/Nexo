import type { Page } from '@playwright/test';

import { uniqueKey, uniqueName } from './support/api';
import { anonymous, expect, loginWithForm, test } from './support/fixtures';

/**
 * Replaces the content of the Monaco editor labelled `label` as a user
 * pasting a document does (typing would trigger the auto-indentation and
 * the bracket auto-closing).
 */
async function setEditorText(page: Page, label: string, text: string) {
  await expect(page.locator('.monaco-editor').first()).toBeVisible();
  const editor = page.getByRole('textbox', { name: label });
  await editor.focus();
  await page.keyboard.press('ControlOrMeta+A');
  await editor.evaluate((element, value) => {
    const data = new DataTransfer();
    data.setData('text/plain', value);
    element.dispatchEvent(
      new ClipboardEvent('paste', { clipboardData: data, bubbles: true, cancelable: true }),
    );
  }, text);
}

const policyFor = (bucket: string, actions: string[]) =>
  JSON.stringify(
    {
      Version: '2012-10-17',
      Statement: [
        {
          Sid: 'OnlyThisBucket',
          Effect: 'Allow',
          Action: actions,
          Resource: [`arn:aws:s3:::${bucket}`, `arn:aws:s3:::${bucket}/*`],
        },
      ],
    },
    null,
    2,
  );

test.describe('identities and policies', () => {
  test('TD-P-05 the policy editor blocks invalid JSON, saves a valid policy and it applies', async ({
    page,
    api,
  }) => {
    const bucket = uniqueName('e2e-pol');
    const name = uniqueName('e2e-readonly');
    await api.createBucket(bucket);
    await api.putObject(bucket, 'doc.txt', 'readable');
    try {
      await page.goto('/policies/new');
      await page.getByRole('textbox', { name: 'Name' }).fill(name);
      const save = page.getByRole('button', { name: 'Save' });

      // Malformed JSON: located, and saving is impossible.
      await setEditorText(
        page,
        'Policy document',
        '{\n  "Version": "2012-10-17",\n  "Statement": [\n',
      );
      await expect(
        page.getByRole('alert').filter({ hasText: 'Invalid JSON at line' }),
      ).toBeVisible();
      await expect(save).toBeDisabled();

      // Well-formed but invalid for the engine: the server locates the problem.
      await setEditorText(
        page,
        'Policy document',
        JSON.stringify(
          {
            Version: '2012-10-17',
            Statement: [{ Effect: 'Maybe', Action: 's3:GetObject', Resource: '*' }],
          },
          null,
          2,
        ),
      );
      await save.click();
      await expect(
        page.getByRole('alert').filter({ hasText: /The policy has \d+ problems?/ }),
      ).toBeVisible();
      expect((await api.admin('GET', `/policies/${name}`)).status).toBe(404);

      // A valid policy is saved...
      await setEditorText(
        page,
        'Policy document',
        policyFor(bucket, ['s3:GetObject', 's3:ListBucket']),
      );
      await save.click();
      await expect(page.getByText(`Policy ${name} saved.`)).toBeVisible();
      const stored = await api.call<{
        name: string;
        document: { Statement: { Action: string[] }[] };
      }>('GET', `/policies/${name}`);
      expect(stored.document.Statement[0]?.Action).toEqual(['s3:GetObject', 's3:ListBucket']);

      // ...and enforced for the users it is attached to.
      const user = { accessKey: uniqueKey('reader'), secretKey: 'reader-secret-123' };
      await api.call('POST', '/users', { json: { ...user, policyNames: [name] } });
      const asUser = api.as(user);
      expect((await asUser.s3('GET', `/${bucket}/doc.txt`)).status).toBe(200);
      expect((await asUser.s3('PUT', `/${bucket}/new.txt`, { body: 'x' })).status).toBe(403);
      await api.call('DELETE', `/users/${user.accessKey}`);
    } finally {
      await api.admin('DELETE', `/policies/${name}`);
      await api.deleteBucket(bucket);
    }
  });

  test('TD-P-06 the simulator shows the decision and the deciding statement', async ({
    page,
    api,
    nexo,
  }) => {
    const bucket = uniqueName('e2e-sim');
    await api.createBucket(bucket);
    try {
      await page.goto('/policy-simulator');
      const form = page.getByRole('form', { name: 'Policy simulator' });
      await form
        .getByRole('textbox', { name: 'User or access key' })
        .fill(nexo.users.member.accessKey);
      await form.getByRole('textbox', { name: 'Action' }).fill('s3:GetObject');
      await form.getByRole('textbox', { name: 'Bucket' }).fill(bucket);
      await form.getByRole('textbox', { name: 'Object' }).fill('a.txt');
      await form.getByRole('button', { name: 'Simulate' }).click();
      await expect(page.getByText('Allowed', { exact: true })).toBeVisible();
      await expect(page.getByText('Decided by policy readwrite')).toBeVisible();
      await expect(page.getByText('Deciding statement')).toBeVisible();

      await form.getByRole('textbox', { name: 'Action' }).fill('admin:CreateUser');
      await form.getByRole('button', { name: 'Simulate' }).click();
      await expect(page.getByText('Denied', { exact: true })).toBeVisible();
    } finally {
      await api.deleteBucket(bucket);
    }
  });

  test.describe('restricted user', () => {
    test('a home directory policy (docs/09 §9.3) browses its own folder from the console', async ({
      api,
      browser,
      nexo,
    }) => {
      const bucket = uniqueName('e2e-homes');
      const policy = uniqueName('home-dir');
      const user = { accessKey: uniqueKey('gina'), secretKey: 'gina-secret-123' };
      const home = `home/${user.accessKey}/`;
      await api.createBucket(bucket);
      await api.putObject(bucket, `${home}notes.txt`, 'mine');
      await api.putObject(bucket, 'home/someone-else/secret.txt', 'not mine');
      try {
        await api.call('PUT', `/policies/${policy}`, {
          json: {
            document: {
              Version: '2012-10-17',
              Statement: [
                {
                  Effect: 'Allow',
                  Action: ['s3:ListBucket'],
                  Resource: [`arn:aws:s3:::${bucket}`],
                  Condition: {
                    StringLike: { 's3:prefix': ['home/${aws:username}/*', 'home/${aws:username}/'] },
                  },
                },
                {
                  Effect: 'Allow',
                  Action: ['s3:GetObject', 's3:PutObject', 's3:DeleteObject'],
                  Resource: [`arn:aws:s3:::${bucket}/home/\${aws:username}/*`],
                },
              ],
            },
          },
        });
        await api.call('POST', '/users', { json: { ...user, policyNames: [policy] } });
        const context = await browser.newContext({ baseURL: nexo.consoleURL, storageState: anonymous });
        try {
          const page = await context.newPage();
          await page.goto('/login');
          await loginWithForm(page, user);
          // The bucket is listed and its page opens on the root, which the
          // user may not list: the console offers to open a folder by path.
          await page.goto('/buckets');
          const table = page.getByRole('table', { name: 'Buckets' });
          await table.getByRole('link', { name: bucket, exact: true }).click();
          const alert = page.getByRole('alert').filter({ hasText: 'You may not list this folder.' });
          await expect(alert).toBeVisible();
          await alert.getByRole('textbox', { name: 'Folder path' }).fill(home);
          await alert.getByRole('button', { name: 'Open folder' }).click();
          await expect(page.getByRole('button', { name: 'notes.txt', exact: true })).toBeVisible();
          await expect(page.getByRole('button', { name: 'Upload' })).toBeVisible();
          // Nothing of the other homes.
          expect(
            (await context.request.get(`/api/v1/buckets/${bucket}/objects`, { params: { prefix: 'home/someone-else/' } })).status(),
          ).toBe(403);
        } finally {
          await context.close();
        }
      } finally {
        await api.admin('DELETE', `/users/${user.accessKey}`);
        await api.admin('DELETE', `/policies/${policy}`);
        await api.deleteBucket(bucket);
      }
    });

    test('TD-E-12 a custom policy limits the user to one bucket and hides the admin menu', async ({
      page,
      api,
      browser,
      nexo,
    }) => {
      const allowed = uniqueName('e2e-x');
      const other = uniqueName('e2e-y');
      const policy = uniqueName('only-x');
      const user = { accessKey: uniqueKey('onlyx'), secretKey: 'only-x-secret-123' };
      await api.createBucket(allowed);
      await api.createBucket(other);
      await api.putObject(allowed, 'hello.txt', 'hi');
      try {
        // Policy and user are created from the console as root.
        await page.goto('/policies/new');
        await page.getByRole('textbox', { name: 'Name' }).fill(policy);
        await setEditorText(page, 'Policy document', policyFor(allowed, ['s3:*']));
        await page.getByRole('button', { name: 'Save' }).click();
        await expect(page.getByText(`Policy ${policy} saved.`)).toBeVisible();

        await page.goto('/users');
        await page.getByRole('button', { name: 'Create user' }).click();
        const create = page.getByRole('form', { name: 'Create user' });
        await create.getByRole('textbox', { name: 'Access key' }).fill(user.accessKey);
        await create.getByRole('textbox', { name: 'Secret key' }).fill(user.secretKey);
        await create.getByRole('textbox', { name: 'Policies' }).fill(policy);
        await page.getByRole('option', { name: policy }).click();
        // Closes the options before submitting.
        await create.getByRole('textbox', { name: 'Access key' }).click();
        await expect(page.getByRole('option', { name: policy })).toBeHidden();
        await create.getByRole('button', { name: 'Create' }).click();
        await expect(page.getByRole('table').getByText(user.accessKey)).toBeVisible();

        // The user signs in: only the bucket of the policy, no admin pages.
        const context = await browser.newContext({
          baseURL: nexo.consoleURL,
          storageState: anonymous,
        });
        try {
          const userPage = await context.newPage();
          await userPage.goto('/login');
          await loginWithForm(userPage, user);
          const nav = userPage.getByRole('navigation', { name: 'Main navigation' });
          await expect(nav.getByRole('link', { name: 'Buckets' })).toBeVisible();
          for (const adminItem of [
            'Users',
            'Groups',
            'Policies',
            'Policy simulator',
            'Configuration',
          ]) {
            await expect(nav.getByRole('link', { name: adminItem })).toBeHidden();
          }
          await userPage.goto('/buckets');
          const table = userPage.getByRole('table', { name: 'Buckets' });
          await expect(table.getByRole('link', { name: allowed, exact: true })).toBeVisible();
          await expect(table.getByRole('link', { name: other, exact: true })).toBeHidden();
          await table.getByRole('link', { name: allowed, exact: true }).click();
          await expect(
            userPage.getByRole('button', { name: 'hello.txt', exact: true }),
          ).toBeVisible();
          // The admin pages are refused by the server too.
          expect((await context.request.get('/api/v1/users')).status()).toBe(403);
        } finally {
          await context.close();
        }
      } finally {
        await api.admin('DELETE', `/users/${user.accessKey}`);
        await api.admin('DELETE', `/policies/${policy}`);
        await api.deleteBucket(allowed);
        await api.deleteBucket(other);
      }
    });
  });

  test('TD-E-13 an access key with an expiration shows its secret once and works with S3', async ({
    page,
    api,
  }) => {
    await page.goto('/access-keys');
    await page.getByRole('button', { name: 'Create access key' }).click();
    const form = page.getByRole('dialog', { name: 'Create access key' });
    await form.getByRole('textbox', { name: 'Name' }).fill('e2e ci key');
    await form.getByText('Expires', { exact: true }).click();
    await form.getByRole('button', { name: 'Expiration' }).click();
    // Tomorrow (00:00).
    const tomorrow = new Date(Date.now() + 24 * 3600 * 1000);
    const dayLabel = tomorrow.toLocaleDateString('en-GB', {
      day: 'numeric',
      month: 'long',
      year: 'numeric',
      timeZone: 'UTC',
    });
    if (tomorrow.getUTCMonth() !== new Date().getUTCMonth()) {
      await page.getByRole('button', { name: 'Next month' }).click();
    }
    await page.getByRole('button', { name: dayLabel, exact: true }).click();
    await page.getByRole('button', { name: 'Confirm the date' }).click();
    await form.getByRole('button', { name: 'Create' }).click();

    const reveal = page.getByRole('dialog', { name: 'Access key created' });
    await expect(reveal.getByText('This is the only time the secret key is shown.')).toBeVisible();
    const accessKey = (await reveal.getByLabel('Access key', { exact: true }).innerText()).trim();
    const secretKey = (await reveal.getByLabel('Secret key', { exact: true }).innerText()).trim();
    expect(secretKey.length).toBeGreaterThanOrEqual(8);
    const done = reveal.getByRole('button', { name: 'Done' });
    await expect(done).toBeDisabled();
    await reveal.getByText('I have saved the secret key').click();
    await done.click();
    await expect(reveal).toBeHidden();

    // The secret is never shown again; the key lists its expiration.
    const row = page.getByRole('row').filter({ hasText: accessKey });
    await expect(row).toBeVisible();
    await expect(row).not.toContainText('Never');
    await expect(page.getByText(secretKey)).toHaveCount(0);
    const listed = await api.call<{ accessKey: string; expiration?: string; secretKey?: string }>(
      'GET',
      `/access-keys/${accessKey}`,
    );
    expect(listed.secretKey ?? '').toBe('');
    expect(Date.parse(listed.expiration ?? '')).toBeGreaterThan(Date.now());

    // The key signs S3 requests (SigV4, like the SDKs).
    const res = await api.as({ accessKey, secretKey }).s3('GET', '/');
    expect(res.status).toBe(200);
    expect(await res.text()).toContain('<ListAllMyBucketsResult');
    await api.call('DELETE', `/access-keys/${accessKey}`);
  });
});
