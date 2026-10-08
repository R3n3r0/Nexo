import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

import type { Page } from '@playwright/test';

/**
 * Writes a folder named `name` under `root` with `count` small files spread
 * over nested sub-folders, and returns the relative paths of the files
 * (sorted), e.g. "album/l1-0/l2-1/file-07.txt".
 */
export async function writeNestedFolder(root: string, name: string, count: number) {
  const paths: string[] = [];
  for (let i = 0; i < count; i++) {
    const segments = [name];
    if (i % 5 !== 0) segments.push(`l1-${i % 3}`);
    if (i % 2 === 1) segments.push(`l2-${i % 4}`);
    segments.push(`file-${String(i).padStart(2, '0')}.txt`);
    const relative = segments.join('/');
    await mkdir(dirname(join(root, relative)), { recursive: true });
    await writeFile(join(root, relative), `content of ${relative}\n`);
    paths.push(relative);
  }
  return paths.sort();
}

/**
 * Drops files or folders of the local disk on the element under the
 * selector, as the operating system would (Chromium drag data with file
 * names): folders arrive as directory entries (webkitGetAsEntry).
 */
export async function dropFromDisk(page: Page, selector: string, paths: string[]) {
  const box = await page.locator(selector).boundingBox();
  if (box === null) {
    throw new Error(`${selector} is not visible`);
  }
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  const cdp = await page.context().newCDPSession(page);
  const data = { items: [], files: paths, dragOperationsMask: 1 };
  for (const type of ['dragEnter', 'dragOver', 'drop'] as const) {
    await cdp.send('Input.dispatchDragEvent', { type, x, y, data });
  }
  await cdp.detach();
}
