// Renders the GitHub social preview card (1280x640) to docs/images/social-preview.png.
// Usage: node scripts/social/render.mjs  (run from the repository root; needs web/node_modules)
import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';

const require = createRequire(resolve('web/package.json'));
const { chromium } = require('@playwright/test');

const shot = readFileSync('docs/images/screenshots/overview-dark.png').toString('base64');
const html = readFileSync('scripts/social/card.html', 'utf8').replace(
  'SCREENSHOT',
  `data:image/png;base64,${shot}`,
);
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 640 } });
await page.setContent(html, { waitUntil: 'load' });
writeFileSync('docs/images/social-preview.png', await page.screenshot({ type: 'png' }));
await browser.close();
console.log('wrote docs/images/social-preview.png');
