import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { uniqueName } from './support/api';
import { repoRoot } from './support/env';
import { expect, test } from './support/fixtures';

/** An MP4 video (H.264 + AAC, 4 s) of the packager fixtures. */
const video = readFileSync(join(repoRoot, 'internal/service/media/testdata/h264_aac.mp4'));

test.describe('video streaming', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-video');
    await api.createBucket(bucket);
    await api.putObject(bucket, 'clips/intro.mp4', video, 'video/mp4');
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  // The Chromium of Playwright has no H.264 decoder: the tests check the
  // HLS requests, the decoding is checked with ffprobe (test/integration).
  test('the console previews a video as an HLS stream', async ({ page }) => {
    const playlists: number[] = [];
    page.on('response', (res) => {
      if (res.url().includes('/objects/hls/master.m3u8')) playlists.push(res.status());
    });
    await page.goto(`/buckets/${bucket}/browse/clips/`);
    await page.getByRole('button', { name: 'intro.mp4', exact: true }).click();
    const drawer = page.getByRole('dialog', { name: 'intro.mp4' });
    await drawer.getByRole('tab', { name: 'Preview' }).click();
    await expect(drawer.getByLabel('Video preview of intro.mp4')).toBeVisible();
    await expect.poll(() => playlists).toContain(200);
  });

  test('a stream link opens the public player anonymously', async ({ page, browser, nexo }) => {
    await page.goto(`/buckets/${bucket}/browse/clips/`);
    await page.getByRole('button', { name: 'Actions for intro.mp4' }).click();
    await page.getByRole('menuitem', { name: 'Share' }).click();
    const dialog = page.getByRole('dialog', { name: `Share ${bucket}/clips/intro.mp4` });
    await expect(dialog.getByRole('switch', { name: /Stream as a video/ })).toBeChecked();
    await dialog.getByText('1 hour', { exact: true }).click();
    await dialog.getByRole('button', { name: 'Create link' }).click();
    const player = await dialog.getByRole('textbox', { name: 'Player link' }).inputValue();
    expect(player.startsWith(`${nexo.s3URL}/nexo/media/player/#/nexo/media/v1/${bucket}/`)).toBe(
      true,
    );

    // An anonymous browser loads the player, its script and the playlist
    // signed by the share.
    const anonymous = await browser.newContext();
    const viewer = await anonymous.newPage();
    const master = viewer.waitForResponse((res) => res.url().includes('/master.m3u8'));
    await viewer.goto(player);
    expect((await master).status()).toBe(200);
    await expect(viewer.locator('video')).toBeVisible();
    await anonymous.close();
  });
});
