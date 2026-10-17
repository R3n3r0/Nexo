import { readdirSync } from 'node:fs';
import { join } from 'node:path';

import { type Page, expect, test } from '@playwright/test';

/**
 * The demo animation of the README (docs/images/demo.gif): sign in, the
 * overview, a new bucket, an upload of a few photos, a preview and a share
 * link. Recorded as a video by Playwright against the demo deployment of
 * scripts/screenshots (`make demo-gif`, which turns the video into the
 * GIF). See playwright.config.ts in this directory for the environment;
 * DEMO_VIDEO_DIR is where the video goes, DEMO_PHOTOS a directory of JPEG
 * files to upload.
 */

const user = process.env.DEMO_ROOT_USER ?? 'admin';
const password = process.env.DEMO_ROOT_PASSWORD ?? 'demo-password-2026';
const videoDir = process.env.DEMO_VIDEO_DIR ?? join(import.meta.dirname, '../test-results/demo');
const photos = process.env.DEMO_PHOTOS ?? '';
const size = { width: 1280, height: 800 };

/** A pause the viewer can follow. */
async function beat(page: Page, ms = 900): Promise<void> {
  await page.waitForTimeout(ms);
}

/** Moves the mouse to the element and clicks it, as a person would. */
async function press(page: Page, target: ReturnType<Page['getByRole']>): Promise<void> {
  const box = await target.boundingBox();
  if (box !== null) {
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 12 });
  }
  await target.click();
}

test('demo', async ({ browser }) => {
  test.setTimeout(180_000);
  const context = await browser.newContext({
    viewport: size,
    recordVideo: { dir: videoDir, size },
    colorScheme: 'light',
  });
  await context.addInitScript(() => {
    localStorage.removeItem('mantine-color-scheme-value');
    document.addEventListener('DOMContentLoaded', () => {
      const style = document.createElement('style');
      // The screencast only sends a frame when the page paints: an invisible
      // animation keeps it painting, the still moments are not lost.
      style.textContent = `
        * { caret-color: transparent !important; }
        @keyframes demo-tick { from { opacity: 0; } to { opacity: 0.01; } }
        body::after {
          content: ''; position: fixed; right: 0; bottom: 0; width: 1px; height: 1px;
          background: #000; pointer-events: none; animation: demo-tick 80ms infinite alternate;
        }`;
      document.head.appendChild(style);
    });
  });
  // The demo drives are directories of the recording machine: shown as four
  // drives of 1 TiB, as in the screenshots (screenshots.spec.ts).
  await context.route('**/api/v1/info', async (route) => {
    const res = await route.fetch();
    const info = (await res.json()) as {
      usage?: number;
      nodes?: {
        disks: { path: string; endpoint: string; total: number; used: number; free: number }[];
      }[];
    };
    const disks = (info.nodes ?? []).flatMap((n) => n.disks);
    disks.forEach((disk, i) => {
      const path = `/mnt/disk${String(i + 1)}`;
      if (disk.endpoint === disk.path) disk.endpoint = path;
      disk.path = path;
      disk.total = 2 ** 40;
      disk.used = Math.round(((info.usage ?? 0) * 2) / Math.max(1, disks.length));
      disk.free = disk.total - disk.used;
    });
    await route.fulfill({ response: res, json: info });
  });
  const page = await context.newPage();

  // Sign in.
  await page.goto('/login');
  await expect(page.getByRole('heading', { name: 'Sign in to Nexo' })).toBeVisible();
  await beat(page, 600);
  await page.getByRole('textbox', { name: 'Access key' }).pressSequentially(user, { delay: 60 });
  await page
    .getByRole('textbox', { name: 'Secret key' })
    .pressSequentially(password, { delay: 40 });
  await press(page, page.getByRole('button', { name: 'Sign in' }));

  // The overview of the deployment.
  await expect(page.getByRole('heading', { name: 'Overview', level: 2 })).toBeVisible();
  await expect(page.locator('.recharts-bar-rectangle').first()).toBeVisible();
  await beat(page, 2200);

  // A new bucket, with versioning.
  await page.goto('/buckets');
  await expect(page.getByRole('table', { name: 'Buckets' })).toBeVisible();
  await beat(page, 800);
  await press(page, page.getByRole('button', { name: 'Create bucket' }));
  const create = page.getByRole('dialog', { name: 'Create a bucket' });
  await create.getByRole('textbox', { name: 'Name' }).pressSequentially('holiday-photos', {
    delay: 70,
  });
  await create.getByText('Versioning', { exact: true }).click();
  await beat(page, 500);
  await press(page, create.getByRole('button', { name: 'Create' }));
  await expect(page.getByRole('heading', { name: 'holiday-photos' })).toBeVisible();
  await beat(page);

  // Upload a few photos.
  const files = readdirSync(photos)
    .filter((name) => name.endsWith('.jpg'))
    .sort()
    .slice(0, 6)
    .map((name) => join(photos, name));
  await press(page, page.getByRole('button', { name: 'Upload' }));
  const chooser = page.waitForEvent('filechooser');
  await page.getByRole('menuitem', { name: 'Upload files' }).click();
  await (await chooser).setFiles(files);
  const uploads = page.getByRole('region', { name: 'Uploads' });
  await expect(uploads.getByText('Completed')).toHaveCount(files.length, { timeout: 60_000 });
  await beat(page, 1500);
  await press(page, uploads.getByRole('button', { name: 'Clear finished' }));
  await expect(uploads).toBeHidden();
  await beat(page, 600);

  // Preview one of them.
  const first = files[0].split('/').pop() ?? '';
  await press(page, page.getByRole('button', { name: first, exact: true }));
  const details = page.getByRole('dialog', { name: first });
  await expect(details).toBeVisible();
  await beat(page, 700);
  await press(page, details.getByRole('tab', { name: 'Preview' }));
  await expect(details.getByRole('img', { name: `Preview of ${first}` })).toBeVisible();
  await beat(page, 2200);
  await page.keyboard.press('Escape');
  await beat(page, 500);

  // Share it for a day.
  await press(page, page.getByRole('button', { name: `Actions for ${first}` }));
  await page.getByRole('menuitem', { name: 'Share' }).click();
  const share = page.getByRole('dialog', { name: `Share holiday-photos/${first}` });
  await share.getByText('1 day', { exact: true }).click();
  await beat(page, 500);
  await press(page, share.getByRole('button', { name: 'Create link' }));
  await expect(share.getByRole('textbox', { name: 'Link' })).toBeVisible();
  await beat(page, 2500);

  await context.close();
});
