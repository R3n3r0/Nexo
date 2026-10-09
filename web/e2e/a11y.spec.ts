import AxeBuilder from '@axe-core/playwright';
import type { Page } from '@playwright/test';

import { uniqueName } from './support/api';
import { anonymous, expect, test } from './support/fixtures';

/** Runs axe-core on the page and returns the serious and critical violations. */
async function seriousViolations(page: Page) {
  const results = await new AxeBuilder({ page }).analyze();
  return results.violations
    .filter((v) => v.impact === 'serious' || v.impact === 'critical')
    .map((v) => ({
      rule: v.id,
      impact: v.impact,
      help: v.help,
      targets: v.nodes.slice(0, 5).map((n) => `${n.target.join(' ')} :: ${n.failureSummary ?? ''}`),
    }));
}

/** Waits until the page settled: no request in flight and no loader shown. */
async function settle(page: Page) {
  await page.waitForLoadState('networkidle');
  await expect(page.locator('.mantine-Loader-root')).toHaveCount(0);
  await expect(page.locator('.mantine-Skeleton-root')).toHaveCount(0);
}

test.describe('TD-E-19 accessibility (axe-core: no serious or critical violation)', () => {
  test.describe('signed out', () => {
    test.use({ storageState: anonymous });

    test('login page', async ({ page }) => {
      await page.goto('/login');
      await settle(page);
      expect(await seriousViolations(page)).toEqual([]);
      // The error state too.
      await page.getByRole('textbox', { name: 'Access key' }).fill('nobody-here');
      await page.getByRole('textbox', { name: 'Secret key' }).fill('wrong-secret-0');
      await page.getByRole('button', { name: 'Sign in' }).click();
      await expect(page.getByRole('alert')).toBeVisible();
      expect(await seriousViolations(page)).toEqual([]);
    });
  });

  test.describe('signed in', () => {
    let bucket: string;

    test.beforeAll(async ({ api }) => {
      bucket = uniqueName('e2e-a11y');
      await api.createBucket(bucket, { versioning: true });
      await api.putObject(bucket, 'docs/readme.txt', 'accessible', 'text/plain');
      await api.putObject(bucket, 'image.png', Buffer.from('not really a png'), 'image/png');
    });

    test.afterAll(async ({ api }) => {
      await api.deleteBucket(bucket);
    });

    const pages = (b: () => string) =>
      [
        ['overview', '/'],
        ['buckets', '/buckets'],
        ['object browser', () => `/buckets/${b()}/browse/`],
        ['object browser folder', () => `/buckets/${b()}/browse/docs/`],
        ['bucket settings', () => `/buckets/${b()}/settings`],
        ['bucket access', () => `/buckets/${b()}/access`],
        ['objects', '/objects'],
        ['users', '/users'],
        ['groups', '/groups'],
        ['policies', '/policies'],
        ['policy editor', '/policies/new'],
        ['built-in policy', '/policies/readonly'],
        ['policy simulator', '/policy-simulator'],
        ['access keys', '/access-keys'],
        ['shares', '/shares'],
        ['configuration', '/configuration'],
        ['info', '/info'],
        ['distributions', '/distributions'],
        ['cache', '/cache'],
        ['cluster', '/cluster'],
        ['healing', '/healing'],
        ['monitoring', '/monitoring'],
        ['not found', '/no/such/page'],
      ] as const;

    for (const [name, target] of pages(() => bucket)) {
      test(name, async ({ page }) => {
        await page.goto(typeof target === 'function' ? target() : target);
        await settle(page);
        expect(await seriousViolations(page)).toEqual([]);
      });
    }

    test('dialogs and drawers', async ({ page }) => {
      await page.goto('/buckets');
      await settle(page);
      await page.getByRole('button', { name: 'Create bucket' }).click();
      await expect(page.getByRole('dialog', { name: 'Create a bucket' })).toBeVisible();
      expect(await seriousViolations(page), 'create bucket dialog').toEqual([]);
      await page.keyboard.press('Escape');

      await page.goto(`/buckets/${bucket}/browse/docs/`);
      await settle(page);
      await page.getByRole('button', { name: 'readme.txt', exact: true }).click();
      await expect(page.getByRole('dialog', { name: 'readme.txt' })).toBeVisible();
      await settle(page);
      expect(await seriousViolations(page), 'object drawer').toEqual([]);
      await page.getByRole('tab', { name: 'Versions' }).click();
      await settle(page);
      expect(await seriousViolations(page), 'object versions').toEqual([]);
      await page.getByRole('button', { name: 'Close' }).click();

      await page.getByRole('button', { name: 'Actions for readme.txt' }).click();
      await page.getByRole('menuitem', { name: 'Share' }).click();
      await expect(page.getByRole('dialog', { name: /^Share / })).toBeVisible();
      expect(await seriousViolations(page), 'share dialog').toEqual([]);
      await page.keyboard.press('Escape');

      await page.goto('/users');
      await settle(page);
      await page.getByRole('button', { name: 'Create user' }).click();
      await expect(page.getByRole('dialog', { name: 'Create user' })).toBeVisible();
      expect(await seriousViolations(page), 'create user dialog').toEqual([]);
      await page.keyboard.press('Escape');

      await page.goto('/access-keys');
      await settle(page);
      await page.getByRole('button', { name: 'Create access key' }).click();
      await expect(page.getByRole('dialog', { name: 'Create access key' })).toBeVisible();
      expect(await seriousViolations(page), 'create access key dialog').toEqual([]);
    });

    test('dark colour scheme', async ({ page }) => {
      await page.goto('/');
      await page.getByRole('button', { name: 'Toggle colour scheme' }).click();
      await expect(page.locator('html')).toHaveAttribute('data-mantine-color-scheme', 'dark');
      for (const target of ['/', '/buckets', `/buckets/${bucket}/browse/`, '/users', '/policies']) {
        await page.goto(target);
        await settle(page);
        expect(await seriousViolations(page), target).toEqual([]);
      }
    });
  });
});
