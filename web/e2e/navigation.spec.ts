import { uniqueName } from './support/api';
import { rootCredentials } from './support/nexo';
import { anonymous, expect, loginWithForm, test } from './support/fixtures';

test.describe('deep links', () => {
  test.use({ storageState: anonymous });

  // covers: D-OBJ-01
  test('TD-E-03 a deep link opens the right page after the login (SPA fallback)', async ({
    page,
    api,
    request,
  }) => {
    const bucket = uniqueName('e2e-deep');
    await api.createBucket(bucket);
    await api.putObject(bucket, 'a/b/report.txt', 'quarterly numbers', 'text/plain');
    await api.putObject(bucket, 'a/v1.2/notes.txt', 'dotted folder', 'text/plain');
    try {
      // The Go server answers every console route with the SPA shell.
      for (const route of [`/buckets/${bucket}/browse/a/b/`, `/buckets/${bucket}/browse/a/v1.2/`]) {
        const res = await request.get(route);
        expect(res.status(), route).toBe(200);
        expect(res.headers()['content-type'], route).toContain('text/html');
        expect(await res.text(), route).toContain('<div id="root">');
      }

      await page.goto(`/buckets/${bucket}/browse/a/b/`);
      await expect(page).toHaveURL(/\/login$/);
      await loginWithForm(page, rootCredentials);
      await expect(page).toHaveURL(new RegExp(`/buckets/${bucket}/browse/a/b/$`));
      await expect(page.getByRole('heading', { name: bucket })).toBeVisible();
      const crumbs = page.getByRole('navigation', { name: 'Folder path' });
      await expect(crumbs).toContainText('a');
      await expect(crumbs).toContainText('b');
      await expect(page.getByRole('button', { name: 'report.txt', exact: true })).toBeVisible();

      // A reload of a nested route keeps the page; so does a dotted folder.
      await page.reload();
      await expect(page.getByRole('button', { name: 'report.txt', exact: true })).toBeVisible();
      await page.goto(`/buckets/${bucket}/browse/a/v1.2/`);
      await expect(page.getByRole('button', { name: 'notes.txt', exact: true })).toBeVisible();

      // The open object is part of the link too.
      await page.goto(
        `/buckets/${bucket}/browse/a/b/?object=${encodeURIComponent('a/b/report.txt')}`,
      );
      await expect(page.getByRole('dialog', { name: 'report.txt' })).toBeVisible();
    } finally {
      await api.deleteBucket(bucket);
    }
  });
});

// covers: D-UX-01
test('TD-E-18 dark theme, English and a 375 px viewport without horizontal overflow', async ({
  browser,
  nexo,
  api,
}) => {
  const bucket = uniqueName('e2e-mobile');
  await api.createBucket(bucket);
  await api.putObject(bucket, 'a-rather-long-folder-name/with-a-long-file-name-2026.txt', 'x');
  const context = await browser.newContext({
    baseURL: nexo.consoleURL,
    storageState: nexo.rootStorageState,
    viewport: { width: 375, height: 740 },
    locale: 'it-IT',
    isMobile: true,
    hasTouch: true,
  });
  try {
    const page = await context.newPage();
    await page.goto('/');
    // The browser language is Italian: switch to English and to the dark theme.
    await expect(page.getByRole('heading', { name: 'Panoramica', level: 2 })).toBeVisible();
    await page.getByRole('radiogroup', { name: 'Lingua' }).getByText('EN').click();
    await expect(page.getByRole('heading', { name: 'Overview', level: 2 })).toBeVisible();
    await page.getByRole('button', { name: 'Toggle colour scheme' }).click();
    await expect(page.locator('html')).toHaveAttribute('data-mantine-color-scheme', 'dark');

    const pages = [
      '/',
      '/buckets',
      `/buckets/${bucket}/browse/`,
      `/buckets/${bucket}/browse/a-rather-long-folder-name/`,
      `/buckets/${bucket}/settings`,
      `/buckets/${bucket}/access`,
      '/users',
      '/groups',
      '/policies',
      '/policies/new',
      '/policy-simulator',
      '/access-keys',
      '/shares',
      '/configuration',
      '/info',
    ];
    for (const path of pages) {
      await page.goto(path);
      await page.waitForLoadState('networkidle');
      // The choices survive the navigation.
      await expect(page.locator('html')).toHaveAttribute('data-mantine-color-scheme', 'dark');
      await expect(page.locator('html')).toHaveAttribute('lang', 'en');
      const overflow = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        client: document.documentElement.clientWidth,
      }));
      expect(overflow.scroll, `horizontal overflow on ${path}`).toBeLessThanOrEqual(
        overflow.client,
      );
    }
    // The navigation is behind the burger on a phone.
    await page.getByRole('button', { name: 'Open navigation' }).click();
    await page.getByRole('link', { name: 'Buckets' }).click();
    await expect(page).toHaveURL(/\/buckets$/);
  } finally {
    await context.close();
    await api.deleteBucket(bucket);
  }
});
