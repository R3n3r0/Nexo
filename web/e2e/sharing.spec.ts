import type { Browser, Page } from '@playwright/test';

import { uniqueName } from './support/api';
import { contextAs, expect, test } from './support/fixtures';

/** GET of a URL from a fresh browser context without any session. */
async function anonymousGet(browser: Browser, url: string) {
  const context = await browser.newContext();
  try {
    const res = await context.request.get(url, { failOnStatusCode: false });
    return { status: res.status(), body: await res.text() };
  } finally {
    await context.close();
  }
}

/** Creates a share link of an object through the share dialog and returns the URL. */
async function shareObject(page: Page, bucket: string, folder: string, name: string) {
  await page.goto(`/buckets/${bucket}/browse/${folder}`);
  await page.getByRole('button', { name: `Actions for ${name}` }).click();
  await page.getByRole('menuitem', { name: 'Share' }).click();
  const dialog = page.getByRole('dialog', { name: `Share ${bucket}/${folder}${name}` });
  await dialog.getByText('1 hour', { exact: true }).click();
  await dialog.getByRole('button', { name: 'Create link' }).click();
  const link = dialog.getByRole('textbox', { name: 'Link' });
  await expect(link).toHaveValue(/^http/);
  const url = await link.inputValue();
  await expect(
    dialog.getByText('Share link created. You can revoke it at any time from the Shares page.'),
  ).toBeVisible();
  await dialog.getByRole('button', { name: 'Close' }).first().click();
  return url;
}

test.describe('sharing', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-share');
    await api.createBucket(bucket);
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  test('TD-E-09 TD-P-01 TD-P-02 a share link opens anonymously until it is revoked', async ({
    page,
    browser,
    api,
    nexo,
  }) => {
    await api.putObject(bucket, 'docs/report.txt', 'shared content 42', 'text/plain');
    const url = await shareObject(page, bucket, 'docs/', 'report.txt');
    expect(url.startsWith(nexo.s3URL)).toBe(true);

    // An anonymous browser opens the link and gets the file.
    const anonymous = await browser.newContext();
    const anonymousPage = await anonymous.newPage();
    const res = await anonymousPage.goto(url);
    expect(res?.status()).toBe(200);
    await expect(anonymousPage.locator('body')).toContainText('shared content 42');
    await anonymous.close();

    // Revoked from the Shares page, the link answers 403.
    await page.goto('/shares');
    const name = `${bucket}/docs/report.txt`;
    const table = page.getByRole('table', { name: 'Active links' });
    await expect(table.getByText(name)).toBeVisible();
    await table.getByRole('button', { name: `Revoke the link of ${name}` }).click();
    const confirm = page.getByRole('dialog', { name: 'Revoke share links' });
    await confirm.getByRole('button', { name: 'Revoke' }).click();
    await expect(table.getByText(name)).toBeHidden();
    await expect.poll(async () => (await anonymousGet(browser, url)).status).toBe(403);
  });

  // Regression (D-SHR-01): sharing a folder showed the ListObjectsV2 URL
  // of the prefix, an XML listing that downloads nothing.
  test('TD-P-01 a shared folder hands out one working link per object', async ({
    page,
    browser,
    api,
  }) => {
    await api.putObject(bucket, 'folder/a.txt', 'first file', 'text/plain');
    await api.putObject(bucket, 'folder/sub/b.txt', 'second file', 'text/plain');
    await page.goto(`/buckets/${bucket}/browse/`);
    await page.getByRole('button', { name: 'Actions for folder/' }).click();
    await page.getByRole('menuitem', { name: 'Share' }).click();
    const dialog = page.getByRole('dialog', { name: `Share ${bucket}/folder/` });
    await dialog.getByText('1 hour', { exact: true }).click();
    await dialog.getByRole('button', { name: 'Create link' }).click();
    const links = dialog.getByRole('table', { name: 'Links of the shared objects' });
    await expect(links).toBeVisible();
    await expect(dialog.getByRole('textbox', { name: 'Link' })).toHaveCount(0);
    for (const [key, body] of [
      ['folder/a.txt', 'first file'],
      ['folder/sub/b.txt', 'second file'],
    ] as const) {
      const href = await links.getByRole('link', { name: key }).getAttribute('href');
      expect(href).not.toBeNull();
      const res = await anonymousGet(browser, href ?? '');
      expect(res).toEqual({ status: 200, body });
    }
  });

  test('TD-P-03 the POST policy generator snippet uploads into the prefix', async ({
    page,
    browser,
    api,
  }) => {
    await page.goto('/shares');
    await page.getByRole('tab', { name: 'Upload generator' }).click();
    const form = page.getByRole('form', { name: 'Upload link generator' });
    await form.getByText('HTML form (POST policy)').click();
    await form.getByRole('textbox', { name: 'Bucket' }).click();
    await page.getByRole('option', { name: bucket }).click();
    await form.getByRole('textbox', { name: 'Key prefix' }).fill('incoming/');
    await form.getByRole('button', { name: 'Generate' }).click();

    const snippets = page.getByRole('region', { name: 'Snippets' });
    await snippets.getByRole('tab', { name: 'HTML' }).click();
    const html = await snippets.getByRole('tabpanel', { name: 'HTML' }).locator('pre').innerText();
    expect(html).toContain('<form action=');

    // The snippet, pasted in a page of another site, uploads the file.
    const context = await browser.newContext();
    const visitor = await context.newPage();
    await visitor.setContent(`<!doctype html><html><body>${html}</body></html>`);
    await visitor.locator('input[type="file"]').setInputFiles({
      name: 'hello.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from('uploaded through the form'),
    });
    const answer = visitor.waitForResponse((r) => r.request().method() === 'POST');
    await visitor.getByRole('button', { name: 'Upload' }).click();
    expect((await answer).status()).toBeLessThan(300);
    await context.close();

    await expect.poll(() => api.listKeys(bucket)).toEqual(['incoming/hello.txt']);
    const object = await api.s3('GET', `/${bucket}/incoming/hello.txt`);
    expect(await object.text()).toBe('uploaded through the form');
  });

  test('TD-P-04 the Access tab makes a prefix public, then private again', async ({
    page,
    browser,
    api,
    nexo,
  }) => {
    await api.putObject(bucket, 'pub/logo.txt', 'public bytes', 'text/plain');
    await api.putObject(bucket, 'private/secret.txt', 'private bytes', 'text/plain');
    const publicURL = `${nexo.s3URL}/${bucket}/pub/logo.txt`;
    const privateURL = `${nexo.s3URL}/${bucket}/private/secret.txt`;
    expect((await anonymousGet(browser, publicURL)).status).toBe(403);

    await page.goto(`/buckets/${bucket}/access`);
    const anonymousAccess = page.getByRole('region', { name: 'Anonymous access' });
    await anonymousAccess.getByRole('textbox', { name: 'Prefix' }).fill('pub/');
    await anonymousAccess.getByRole('textbox', { name: 'Access' }).click();
    await page.getByRole('option', { name: 'Download' }).click();
    await anonymousAccess.getByRole('button', { name: 'Apply' }).click();
    const rules = anonymousAccess.getByRole('table', { name: 'Anonymous access rules' });
    await expect(rules.getByText('pub/')).toBeVisible();

    expect(await anonymousGet(browser, publicURL)).toEqual({ status: 200, body: 'public bytes' });
    expect((await anonymousGet(browser, privateURL)).status).toBe(403);

    await rules.getByRole('button', { name: 'Make pub/ private' }).click();
    await expect(
      anonymousAccess.getByText('The bucket is private: anonymous requests are denied.'),
    ).toBeVisible();
    expect((await anonymousGet(browser, publicURL)).status).toBe(403);
  });

  test('TD-P-07 a user without admin:* sees only its own share links', async ({
    page,
    browser,
    api,
    nexo,
  }) => {
    await api.putObject(bucket, 'root-file.txt', 'by root', 'text/plain');
    await api.putObject(bucket, 'member-file.txt', 'by member', 'text/plain');
    const rootURL = await shareObject(page, bucket, '', 'root-file.txt');

    const memberContext = await contextAs(browser, nexo, nexo.users.member);
    try {
      const member = await memberContext.newPage();
      const memberURL = await shareObject(member, bucket, '', 'member-file.txt');
      expect((await anonymousGet(browser, memberURL)).body).toBe('by member');

      await member.goto('/shares');
      const table = member.getByRole('table', { name: 'Active links' });
      await expect(table.getByText(`${bucket}/member-file.txt`)).toBeVisible();
      await expect(table.getByText(`${bucket}/root-file.txt`)).toBeHidden();
      // The API agrees: the listing is filtered by the server.
      const listed = await api
        .as(nexo.users.member)
        .call<{ items: { key: string; owner: string }[] }>('GET', '/shares', {
          query: { bucket },
        });
      expect(listed.items.map((s) => s.key)).toEqual(['member-file.txt']);
      // Root sees both.
      await page.goto('/shares');
      const rootTable = page.getByRole('table', { name: 'Active links' });
      await expect(rootTable.getByText(`${bucket}/member-file.txt`)).toBeVisible();
      await expect(rootTable.getByText(`${bucket}/root-file.txt`)).toBeVisible();
      expect((await anonymousGet(browser, rootURL)).body).toBe('by root');
    } finally {
      await memberContext.close();
    }
  });
});
