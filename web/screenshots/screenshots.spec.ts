import { mkdirSync } from 'node:fs';
import { join } from 'node:path';

import {
  type Browser,
  type BrowserContext,
  type Locator,
  type Page,
  expect,
  test,
} from '@playwright/test';

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

/** The demo video of seed.sh and its stream link (run.sh exports the player page). */
const videoBucket = 'videos';
const videoKey = 'trailers/aurora.mp4';
const playerUrl = process.env.SCREENSHOTS_PLAYER_URL;

/**
 * A still of the demo video, selected by the image rule "stills" of the
 * video bucket (every format at 640 pixels and its own size: the variants and
 * the preview fit in the drawer).
 */
const imageKey = 'stills/aurora-still.jpg';

/** Whether an object exists and the media worker has processed it. */
async function mediaProcessed(
  context: BrowserContext,
  bucket: string,
  key: string,
): Promise<boolean> {
  const res = await context.request.get(
    `/api/v1/buckets/${bucket}/objects/media/status?key=${encodeURIComponent(key)}`,
  );
  if (!res.ok()) return false;
  const status = (await res.json()) as { state?: string; current?: boolean };
  return status.state === 'done' && status.current === true;
}

/** Waits until an image of the page is loaded. */
async function loaded(image: Locator): Promise<void> {
  await expect(image).toBeVisible();
  await expect
    .poll(() => image.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth))
    .toBeGreaterThan(0);
}

test('video player', async ({ browser }) => {
  test.skip(playerUrl === undefined || playerUrl === '', 'no stream link (seed.sh needs ffmpeg)');
  // The player page of a stream link, served by the S3 listener: 16:9, the
  // video fills it.
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  const page = await context.newPage();
  await page.goto(playerUrl ?? '');
  const h264 = await page.evaluate(() =>
    MediaSource.isTypeSupported('video/mp4; codecs="avc1.640028,mp4a.40.2"'),
  );
  test.skip(!h264, 'this browser has no H.264 decoder: set NEXO_E2E_CHROMIUM to Google Chrome');
  const video = page.locator('#video');
  await expect
    .poll(() => video.evaluate((v: HTMLVideoElement) => v.readyState), { timeout: 30_000 })
    .toBeGreaterThanOrEqual(1);
  // A few seconds in and playing, a frame decoded: the click on the video
  // is the user gesture that the autoplay policy asks for.
  await video.evaluate(async (v: HTMLVideoElement) => {
    v.currentTime = 6;
    await new Promise((resolve) => v.addEventListener('seeked', resolve, { once: true }));
  });
  await video.click({ position: { x: 640, y: 300 } });
  await video.evaluate(async (v: HTMLVideoElement) => {
    if (v.paused) await v.play();
    await new Promise((resolve) => v.requestVideoFrameCallback(resolve));
  });
  await expect
    .poll(() => video.evaluate((v: HTMLVideoElement) => v.currentTime))
    .toBeGreaterThan(6.5);
  // The controls show while the pointer is over the video.
  await page.mouse.move(640, 600);
  await page.waitForTimeout(500);
  await page.screenshot({ path: join(out, 'video-player.png') });
  await context.close();
});

test('media panel', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  test.skip(
    !(await mediaProcessed(context, videoBucket, videoKey)),
    'the demo video has no media worker outputs',
  );
  await page.goto(
    `/buckets/${videoBucket}/browse/trailers/?object=${encodeURIComponent(videoKey)}`,
  );
  const drawer = page.getByRole('dialog');
  await drawer.getByRole('tab', { name: 'Media', exact: true }).click();
  await expect(drawer.getByRole('table')).toContainText('720p');
  await loaded(drawer.locator('img'));
  await shoot(page, 'media-panel');
  await context.close();
});

test('media rules', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  const res = await context.request.get(`/api/v1/buckets/${videoBucket}`);
  test.skip(!res.ok(), 'no video bucket (seed.sh needs ffmpeg)');
  await page.goto(`/buckets/${videoBucket}/media`);
  await expect(page.getByText('social-clips')).toBeVisible();
  await expect(page.getByText('stills', { exact: true })).toBeVisible();
  await shoot(page, 'media-rules');
  await context.close();
});

test('image variants', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  test.skip(
    !(await mediaProcessed(context, videoBucket, imageKey)),
    'the demo still has no variants (seed.sh needs ffmpeg, Pillow and the media worker)',
  );
  await page.goto(`/buckets/${videoBucket}/browse/stills/?object=${encodeURIComponent(imageKey)}`);
  const drawer = page.getByRole('dialog');
  await drawer.getByRole('tab', { name: 'Media', exact: true }).click();
  await expect(drawer.getByRole('table')).toContainText('AVIF');
  await loaded(drawer.locator('img'));
  await shoot(page, 'image-variants');
  await context.close();
});

test('create bucket', async ({ browser }) => {
  const { context, page } = await newSession(browser);
  await page.goto('/buckets');
  await page.getByRole('button', { name: 'Create bucket' }).click();
  const dialog = page.getByRole('dialog', { name: 'Create a bucket' });
  await dialog.getByRole('textbox', { name: 'Name' }).fill('product-photos');
  await dialog.getByText('Versioning', { exact: true }).click();
  await dialog.getByText('Images in every format', { exact: true }).click();
  await expect(dialog.getByRole('switch', { name: /^Images in every format/ })).toBeChecked();
  await dialog.getByRole('heading', { name: 'Create a bucket' }).click();
  await shoot(page, 'create-bucket');
  await context.close();
});
