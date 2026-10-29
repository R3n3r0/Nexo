import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { uniqueName } from './support/api';
import { repoRoot } from './support/env';
import { expect, test } from './support/fixtures';

/** An MP4 video (H.264 + AAC, 4 s) of the packager fixtures. */
const video = readFileSync(join(repoRoot, 'internal/service/media/testdata/h264_aac.mp4'));

interface PlaybackToken {
  token: string;
  playlist: string;
  player: string;
}

interface Protection {
  enabled: boolean;
  mode?: string;
  legacyDevices?: string;
  forensic: boolean;
}

// covers: S-OBJ-18, S-OBJ-19
test.describe('video protection', () => {
  let bucket: string;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-protect');
    await api.createBucket(bucket);
    await api.putObject(bucket, 'courses/intro.mp4', video, 'video/mp4');
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  test('the console turns on the Nexo player only mode and the forensic watermark', async ({
    page,
    api,
  }) => {
    await page.goto(`/buckets/${bucket}/media`);
    const section = page.getByRole('region', { name: 'Video protection' });
    // The inputs of the Mantine switches are visually hidden: their labels
    // toggle them, as a pointer does.
    const protect = section.getByRole('switch', { name: /^Protect the videos/ });
    await section
      .locator('label')
      .filter({ hasText: /^Protect the videos/ })
      .click();
    await expect(protect).toBeChecked();
    await section.getByText('Nexo player only', { exact: true }).click();
    await expect(section.getByText(/negotiates a key per session/)).toBeVisible();
    const forensic = section.getByRole('switch', { name: /^Forensic watermark/ });
    await section
      .locator('label')
      .filter({ hasText: /^Forensic watermark/ })
      .click();
    await expect(forensic).toBeChecked();
    await section.getByRole('button', { name: 'Save protection' }).click();
    await expect(page.getByText('Video protection saved.')).toBeVisible();

    const saved = await api.call<Protection>('GET', `/buckets/${bucket}/media/protection`);
    expect(saved).toMatchObject({
      enabled: true,
      mode: 'advanced',
      legacyDevices: 'refuse',
      forensic: true,
    });
  });

  // The Chromium of Playwright has no H.264 decoder: the test checks the
  // session and the requests of the player, the decryption of the
  // segments is checked in test/integration and in the SDK tests.
  test('only the Nexo player plays an advanced video, through a session', async ({
    browser,
    api,
  }) => {
    await api.call('PUT', `/buckets/${bucket}/media/protection`, {
      json: {
        enabled: true,
        prefix: 'courses/',
        mode: 'advanced',
        watermark: true,
        forensic: false,
      },
    });
    const tok = await api.call<PlaybackToken>(
      'POST',
      `/buckets/${bucket}/objects/playback-tokens`,
      {
        query: { key: 'courses/intro.mp4' },
        json: { subject: 'student-7', watermark: 'Mario Rossi' },
      },
    );

    // Any other player: the token alone opens nothing.
    const plain = await fetch(tok.playlist);
    expect(plain.status).toBe(403);
    expect(plain.headers.get('x-nexo-playback')).toBe('session');

    const anonymous = await browser.newContext();
    const viewer = await anonymous.newPage();
    const requests: string[] = [];
    viewer.on('request', (req) => requests.push(req.url()));
    const frames: string[] = [];
    const sockets: string[] = [];
    viewer.on('websocket', (ws) => {
      sockets.push(ws.url());
      ws.on('framereceived', (f) => frames.push(String(f.payload)));
    });
    const master = viewer.waitForResponse(
      (res) => res.url().includes('/master.m3u8') && res.url().includes('st='),
    );
    await viewer.goto(tok.player);

    expect((await master).status()).toBe(200);
    expect(sockets.filter((u) => u.startsWith('ws') && u.includes('/session?'))).toHaveLength(1);
    await expect.poll(() => frames.join('\n')).toContain('"t":"ready"');
    await expect(viewer.locator('video')).toBeVisible();

    // The keys travel on the channel, never as plain requests.
    expect(requests.filter((u) => /\/nexo\/media\/v1\/[^/]+\/key\?/.test(u))).toEqual([]);
    expect(frames.join('\n')).not.toContain(tok.token);
    await anonymous.close();
  });
});
