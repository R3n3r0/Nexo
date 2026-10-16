import { mkdirSync } from 'node:fs';
import { join } from 'node:path';

import { type Browser, type BrowserContext, type Page, expect, test } from '@playwright/test';

/**
 * Screenshots of the console for the README, taken against the demo
 * deployment seeded by scripts/screenshots/seed.sh (`make screenshots`).
 * See playwright.config.ts in this directory for the environment.
 */

const out =
  process.env.SCREENSHOTS_DIR ?? join(import.meta.dirname, '../../docs/images/screenshots');
const user = process.env.DEMO_ROOT_USER ?? 'admin';
const password = process.env.DEMO_ROOT_PASSWORD ?? 'demo-password-2026';

/**
 * Size shown for each drive (bytes; 0 keeps the real numbers). The demo
 * drives are directories of the machine taking the screenshots: their
 * capacity, free space and paths would be those of its filesystem. The
 * screenshots show them as dedicated drives /mnt/disk1..N of this size,
 * with the space used by Nexo (the data usage of the server spread over the
 * drives, parity included) as their used space.
 */
const driveSize = Number(process.env.SCREENSHOTS_DRIVE_SIZE ?? String(2 ** 40));

mkdirSync(out, { recursive: true });

/** Hides the notifications and the blinking caret: nothing transient in the images. */
const quietStyles = `
  .mantine-Notifications-root { display: none !important; }
  * { caret-color: transparent !important; }
`;

interface Disk {
  path: string;
  endpoint: string;
  total: number;
  used: number;
  free: number;
}
interface Info {
  usage?: number;
  pools?: { drivesPerSet: number; parity: number }[];
  nodes?: { disks: Disk[] }[];
}

async function presentDrives(context: BrowserContext): Promise<void> {
  if (driveSize <= 0) return;
  await context.route('**/api/v1/info', async (route) => {
    const res = await route.fetch();
    const info = (await res.json()) as Info;
    const disks = (info.nodes ?? []).flatMap((n) => n.disks);
    const pool = info.pools?.[0];
    const overhead =
      pool !== undefined && pool.drivesPerSet > pool.parity
        ? pool.drivesPerSet / (pool.drivesPerSet - pool.parity)
        : 1;
    const perDisk = disks.length > 0 ? ((info.usage ?? 0) * overhead) / disks.length : 0;
    disks.forEach((disk, i) => {
      const path = `/mnt/disk${String(i + 1)}`;
      if (disk.endpoint === disk.path) disk.endpoint = path;
      disk.path = path;
      disk.total = driveSize;
      disk.used = Math.round(perDisk);
      disk.free = driveSize - disk.used;
    });
    await route.fulfill({ response: res, json: info });
  });
}

async function newSession(
  browser: Browser,
  colorScheme: 'light' | 'dark' = 'light',
): Promise<{ context: BrowserContext; page: Page }> {
  const context = await browser.newContext({ colorScheme });
  await context.addInitScript((css) => {
    // The colour scheme of the console follows the system (auto).
    localStorage.removeItem('mantine-color-scheme-value');
    document.addEventListener('DOMContentLoaded', () => {
      const style = document.createElement('style');
      style.textContent = css;
      document.head.appendChild(style);
    });
  }, quietStyles);
  await presentDrives(context);
  const res = await context.request.post('/api/v1/auth/login', {
    data: { accessKey: user, secretKey: password },
  });
  expect(res.ok(), `login: ${String(res.status())}`).toBe(true);
  const page = await context.newPage();
  return { context, page };
}

/** Waits until the page has no pending request, skeleton or loader. */
async function settle(page: Page): Promise<void> {
  await page.waitForLoadState('networkidle');
  await expect(page.locator('.mantine-Skeleton-root[data-visible="true"]')).toHaveCount(0);
  await expect(page.locator('.mantine-Loader-root:visible')).toHaveCount(0);
  // Charts animate in, fonts and lazy chunks load.
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(800);
}

async function shoot(page: Page, name: string): Promise<void> {
  await settle(page);
  await page.mouse.move(0, 0);
  await page.screenshot({ path: join(out, `${name}.png`), animations: 'disabled' });
}

test.describe.configure({ mode: 'serial' });

test('login', async ({ browser }) => {
  const context = await browser.newContext();
  await context.addInitScript((css) => {
    document.addEventListener('DOMContentLoaded', () => {
      const style = document.createElement('style');
      style.textContent = css;
      document.head.appendChild(style);
    });
  }, quietStyles);
  const page = await context.newPage();
  await page.goto('/login');
  await page.getByRole('textbox', { name: 'Access key' }).fill(user);
  await page.getByRole('textbox', { name: 'Secret key' }).fill(password);
  await page.getByRole('heading', { name: 'Sign in to Nexo' }).click();
  await shoot(page, 'login');
  await context.close();
});

test('overview', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Overview', level: 2 })).toBeVisible();
  await expect(page.getByText('Storage per bucket')).toBeVisible();
  await expect(page.locator('.recharts-bar-rectangle').first()).toBeVisible();
  await shoot(page, 'overview');
  await context.close();
});

test('object browser', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/buckets/photos/browse/albums/2026/alps-trip/');
  await expect(page.getByRole('button', { name: 'summit-sunrise.jpg', exact: true })).toBeVisible();
  await shoot(page, 'object-browser');
  await context.close();
});

test('bucket lifecycle', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/buckets/photos/lifecycle');
  await expect(page.getByText('keep-3-versions')).toBeVisible();
  await shoot(page, 'bucket-lifecycle');
  await context.close();
});

test('access rules', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/buckets/photos/rules');
  await expect(page.getByText('public-thumbnails')).toBeVisible();
  await shoot(page, 'access-rules');
  await context.close();
});

test('policy editor', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/policies/website-publish');
  await expect(page.locator('.monaco-editor').first()).toBeVisible();
  await expect(page.getByText('NeverTouchBackups').first()).toBeVisible();
  await shoot(page, 'policy-editor');
  await context.close();
});

test('users', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/users');
  await expect(page.getByText('web-deployer', { exact: true })).toBeVisible();
  await shoot(page, 'users');
  await context.close();
});

test('distributions', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/distributions');
  await expect(page.getByText('www.example.com').first()).toBeVisible();
  await shoot(page, 'distributions');
  await context.close();
});

test('cache', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/cache');
  await expect(page.getByText('Top domains')).toBeVisible();
  await expect(page.getByText('www.example.com').first()).toBeVisible();
  await shoot(page, 'cache');
  await context.close();
});

async function metrics(page: Page): Promise<void> {
  await page.goto('/monitoring/metrics?range=15m');
  await expect(page.locator('.recharts-line-curve').first()).toBeVisible();
}

test('monitoring', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await metrics(page);
  await shoot(page, 'monitoring');
  await context.close();
});

test('cluster', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/cluster');
  await expect(page.getByText('Pool 1')).toBeVisible();
  // The pool, its erasure set and its drives at the top of the page.
  await page.getByText('Pool 1').evaluate((el) => {
    el.scrollIntoView({ block: 'start' });
    window.scrollBy(0, -24);
  });
  await shoot(page, 'cluster');
  await context.close();
});

test('dark mode', async ({ browser }) => {
  const { context, page } = await newSession(browser, 'dark');
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Overview', level: 2 })).toBeVisible();
  await expect(page.locator('.recharts-bar-rectangle').first()).toBeVisible();
  await shoot(page, 'overview-dark');
  await context.close();
});
