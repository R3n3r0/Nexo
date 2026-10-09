import type { Browser, Locator, Page } from '@playwright/test';

import { uniqueName } from './support/api';
import { expect, test } from './support/fixtures';

/** The keys of the bucket of each test. */
const keys = ['docs/a.txt', 'img/2026/a.png', 'img/2026/b.JPG', 'img/old/c.png'];

interface Rule {
  id: string;
  priority: number;
  effect: 'allow' | 'deny';
  principals: string[];
  actions: string[];
  match?: { type: 'prefix' | 'glob' | 'regex'; pattern: string; caseInsensitive?: boolean };
}

interface RulesDocument {
  rules: Rule[];
  etag: string;
  public: boolean;
}

interface KeyTest {
  results: { key: string; allowed: boolean; determinant?: string; matched: string[] }[];
}

/** GET of an object of the S3 API from a fresh browser context without any session. */
async function anonymousGet(browser: Browser, url: string) {
  const context = await browser.newContext();
  try {
    const res = await context.request.get(url, { failOnStatusCode: false });
    return { status: res.status(), body: await res.text() };
  } finally {
    await context.close();
  }
}

/** The ids of the rows of the rules table, in their order. */
async function rowOrder(table: Locator): Promise<string[]> {
  return table
    .locator('tbody tr')
    .evaluateAll((rows) => rows.map((r) => r.getAttribute('data-rule') ?? ''));
}

/** Opens the access rules tab of a bucket and waits for the editable table. */
async function openRules(page: Page, bucket: string) {
  await page.goto(`/buckets/${bucket}/rules`);
  await expect(page.getByRole('button', { name: 'Add rule' })).toBeEnabled();
}

/** D-ACC-01..04 against the server (docs/10 §10.6 TD-A-01..03). */
test.describe('access rules', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-rules');
    await api.createBucket(bucket);
    for (const key of keys) {
      await api.putObject(bucket, key, `content of ${key}`, 'text/plain');
    }
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  test('TD-A-01 a regex rule built with the tester: sample keys highlighted, invalid regex blocked', async ({
    page,
    api,
    browser,
    nexo,
  }) => {
    await openRules(page, bucket);
    await expect(page.getByText(/No access rule/)).toBeVisible();
    await page.getByRole('button', { name: 'Add rule' }).click();
    const dialog = page.getByRole('dialog', { name: 'New access rule' });
    await dialog.getByRole('textbox', { name: 'Rule ID' }).fill('img-pub');
    await dialog.getByRole('textbox', { name: 'Pattern type' }).click();
    await page.getByRole('option', { name: 'Regular expression' }).click();
    const pattern = dialog.getByRole('textbox', { name: 'Pattern', exact: true });

    // A regex that does not compile is reported while typing and blocks the save.
    await pattern.fill('^img/(2026');
    await dialog.getByRole('button', { name: 'Save rule' }).click();
    await expect(dialog.getByText('Fix the highlighted fields.')).toBeVisible();
    await expect(pattern).toHaveAttribute('aria-invalid', 'true');
    expect((await api.call<RulesDocument>('GET', `/buckets/${bucket}/access-rules`)).rules).toEqual(
      [],
    );

    // An error only RE2 knows (repeat count over 1000) is located by the server.
    await pattern.fill('^img/x{1001}$');
    await dialog.getByRole('button', { name: 'Save rule' }).click();
    await expect(dialog.getByText('Position 7: invalid repeat count: {1001}')).toBeVisible();
    expect((await api.call<RulesDocument>('GET', `/buckets/${bucket}/access-rules`)).rules).toEqual(
      [],
    );

    // The valid regex: the sample keys of the bucket are matched live.
    await pattern.fill('^img/[0-9]{4}/.*\\.(jpe?g|png)$');
    await dialog
      .locator('label')
      .filter({ hasText: /^Ignore case$/ })
      .click();
    const preview = dialog.getByRole('list', {
      name: 'Sample keys and whether the pattern matches them',
    });
    await expect(preview.getByLabel('img/2026/a.png: matches')).toBeVisible();
    await expect(preview.getByLabel('img/2026/b.JPG: matches')).toBeVisible();
    await expect(preview.getByLabel('img/old/c.png: does not match')).toBeVisible();
    await expect(preview.getByLabel('docs/a.txt: does not match')).toBeVisible();
    await expect(dialog.getByText('2 of 4 keys match.')).toBeVisible();
    await dialog.getByRole('button', { name: 'Save rule' }).click();
    await expect(dialog).toBeHidden();

    const table = page.getByRole('table', { name: 'Access rules of the bucket' });
    await expect(table.getByText('img-pub')).toBeVisible();
    const stored = await api.call<RulesDocument>('GET', `/buckets/${bucket}/access-rules`);
    expect(stored.rules).toEqual([
      expect.objectContaining({
        id: 'img-pub',
        effect: 'allow',
        principals: ['*'],
        actions: ['read'],
        match: { type: 'regex', pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png)$', caseInsensitive: true },
      }),
    ]);

    // The tester of the page evaluates the keys of the bucket on the server.
    const tester = page.getByRole('region', { name: 'Pattern tester' });
    await tester.getByRole('button', { name: 'Suggest keys of the bucket' }).click();
    await expect(tester.getByRole('textbox', { name: 'Keys' })).toHaveValue(keys.join('\n'));
    await tester.getByRole('button', { name: 'Test', exact: true }).click();
    const results = tester.getByRole('list', { name: 'Results of the test' });
    const allowed = results.locator('[data-key="img/2026/b.JPG"]');
    await expect(allowed.getByText('Allowed', { exact: true })).toBeVisible();
    await expect(allowed.getByText('Decided by the rule img-pub.', { exact: false })).toBeVisible();
    await expect(
      results.locator('[data-key="img/old/c.png"]').getByText('Denied', { exact: true }),
    ).toBeVisible();

    // The anonymous requests get what the tester said.
    const url = (key: string) => `${nexo.s3URL}/${bucket}/${key}`;
    expect((await anonymousGet(browser, url('img/2026/b.JPG'))).status).toBe(200);
    expect((await anonymousGet(browser, url('img/old/c.png'))).status).toBe(403);
  });

  test('TD-A-02 the order set by drag and drop persists and decides on the server', async ({
    page,
    api,
  }) => {
    // Two allow rules for the same keys: the one with the lower priority decides.
    const rule = (id: string, priority: number, match: Rule['match']): Rule => ({
      id,
      priority,
      effect: 'allow',
      principals: ['*'],
      actions: ['read'],
      match,
    });
    await api.call('PUT', `/buckets/${bucket}/access-rules`, {
      json: {
        rules: [
          rule('docs-prefix', 10, { type: 'prefix', pattern: 'docs/' }),
          rule('docs-glob', 20, { type: 'glob', pattern: 'docs/**' }),
          rule('images', 30, { type: 'prefix', pattern: 'img/' }),
        ],
      },
    });
    const determinant = async () =>
      (
        await api.call<KeyTest>('POST', `/buckets/${bucket}/access-rules/test`, {
          json: { keys: ['docs/a.txt'], principal: 'anonymous' },
        })
      ).results[0]?.determinant;
    expect(await determinant()).toBe('docs-prefix');

    await openRules(page, bucket);
    const table = page.getByRole('table', { name: 'Access rules of the bucket' });
    await expect.poll(() => rowOrder(table)).toEqual(['docs-prefix', 'docs-glob', 'images']);
    // Drag "docs-glob" onto the first row: it takes its place, saved at once.
    await table
      .locator('tr[data-rule="docs-glob"]')
      .dragTo(table.locator('tr[data-rule="docs-prefix"]'));
    await expect.poll(() => rowOrder(table)).toEqual(['docs-glob', 'docs-prefix', 'images']);
    await expect
      .poll(async () => {
        const stored = await api.call<RulesDocument>('GET', `/buckets/${bucket}/access-rules`);
        return [...stored.rules].sort((a, b) => a.priority - b.priority).map((r) => r.id);
      })
      .toEqual(['docs-glob', 'docs-prefix', 'images']);
    // The server evaluates the new order.
    expect(await determinant()).toBe('docs-glob');

    // The order survives a reload; the keyboard moves the rules as well.
    await page.reload();
    await expect.poll(() => rowOrder(table)).toEqual(['docs-glob', 'docs-prefix', 'images']);
    await table.getByRole('button', { name: 'Move images up' }).click();
    await expect.poll(() => rowOrder(table)).toEqual(['docs-glob', 'images', 'docs-prefix']);
    await expect
      .poll(async () => {
        const stored = await api.call<RulesDocument>('GET', `/buckets/${bucket}/access-rules`);
        return [...stored.rules].sort((a, b) => a.priority - b.priority).map((r) => r.id);
      })
      .toEqual(['docs-glob', 'images', 'docs-prefix']);

    // A change saved elsewhere meanwhile: the save is refused (412), then reloaded.
    const current = await api.call<RulesDocument>('GET', `/buckets/${bucket}/access-rules`);
    await api.call('PUT', `/buckets/${bucket}/access-rules`, {
      json: { rules: [...current.rules, rule('late', 40, { type: 'prefix', pattern: 'late/' })] },
      headers: { 'if-match': `"${current.etag}"` },
    });
    await table.getByRole('button', { name: 'Move docs-prefix up' }).click();
    await expect(page.getByText('The rules changed')).toBeVisible();
    await page.getByRole('button', { name: 'Reload the rules' }).click();
    await expect
      .poll(() => rowOrder(table))
      .toEqual(['docs-glob', 'images', 'docs-prefix', 'late']);
  });

  test('TD-A-03 the public-prefix preset opens the prefix only and marks the bucket public', async ({
    page,
    api,
    browser,
    nexo,
  }) => {
    await openRules(page, bucket);
    const panel = page.getByRole('region', { name: 'Public access' });
    await expect(panel.getByText('Not public')).toBeVisible();
    await panel
      .locator('label')
      .filter({ hasText: /^Public read of a prefix$/ })
      .click();
    await expect(
      panel.getByText('The matching objects become readable without credentials.'),
    ).toBeVisible();
    await panel.getByRole('textbox', { name: 'Public keys' }).fill('img/2026/');
    await panel.getByRole('button', { name: 'Apply “Public read of a prefix”' }).click();
    await expect(panel.getByText('Public', { exact: true })).toBeVisible();
    await expect(
      page
        .getByRole('table', { name: 'Access rules of the bucket' })
        .getByText('preset-public-prefix'),
    ).toBeVisible();
    expect(
      await api.call<{ preset: string; public: boolean }>(
        'GET',
        `/buckets/${bucket}/access-rules/public`,
      ),
    ).toMatchObject({ preset: 'public-prefix', public: true });

    // Anonymous: readable inside the prefix only.
    const url = (key: string) => `${nexo.s3URL}/${bucket}/${key}`;
    const inside = await anonymousGet(browser, url('img/2026/a.png'));
    expect(inside).toEqual({ status: 200, body: 'content of img/2026/a.png' });
    expect((await anonymousGet(browser, url('img/old/c.png'))).status).toBe(403);
    expect((await anonymousGet(browser, url('docs/a.txt'))).status).toBe(403);

    // The bucket list shows the bucket as public.
    await page.goto('/buckets');
    const row = page.getByRole('row').filter({ hasText: bucket });
    await expect(row.getByText('Public', { exact: true })).toBeVisible();

    // Private again: the badge goes and the prefix is closed.
    await openRules(page, bucket);
    await panel
      .locator('label')
      .filter({ hasText: /^Private$/ })
      .click();
    await panel.getByRole('button', { name: 'Apply “Private”' }).click();
    await expect(panel.getByText('Not public')).toBeVisible();
    await expect
      .poll(async () => (await anonymousGet(browser, url('img/2026/a.png'))).status)
      .toBe(403);
    await page.goto('/buckets');
    await expect(row).toBeVisible();
    await expect(row.getByText('Public', { exact: true })).toBeHidden();
  });

  test('D-ACC-04 the block public access of the bucket ignores the public rules', async ({
    page,
    api,
    browser,
    nexo,
  }) => {
    await api.call('PUT', `/buckets/${bucket}/access-rules/public`, {
      json: { preset: 'public-read' },
    });
    const url = `${nexo.s3URL}/${bucket}/docs/a.txt`;
    expect((await anonymousGet(browser, url)).status).toBe(200);
    await openRules(page, bucket);
    const block = page.getByRole('region', { name: 'Block public access' });
    await block
      .locator('label')
      .filter({ hasText: /^Block public access on this bucket$/ })
      .click();
    await expect(block.getByText('Public access blocked', { exact: true })).toBeVisible();
    await expect.poll(async () => (await anonymousGet(browser, url)).status).toBe(403);
    expect(
      await api.call<{ enabled: boolean }>('GET', `/buckets/${bucket}/block-public`),
    ).toMatchObject({ enabled: true });
  });
});
