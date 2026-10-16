import { defineConfig, devices } from '@playwright/test';

/**
 * Screenshots of the console for the README (docs/images/screenshots).
 *
 * Not part of `npm run e2e` (its config only looks in ./e2e): run with
 * `make screenshots`, which starts the demo deployment of
 * scripts/screenshots, seeds it and then runs
 * `npx playwright test -c screenshots/playwright.config.ts`.
 *
 * Environment: DEMO_CONSOLE_URL (console of the demo, default
 * http://127.0.0.1:38001), DEMO_ROOT_USER / DEMO_ROOT_PASSWORD,
 * SCREENSHOTS_DIR (output directory, default docs/images/screenshots),
 * SCREENSHOTS_SCALE (deviceScaleFactor, default 1), NEXO_E2E_CHROMIUM
 * (Chromium executable, optional).
 */
export default defineConfig({
  testDir: '.',
  testMatch: /screenshots\.spec\.ts$/,
  outputDir: '../test-results/screenshots',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 20_000 },
  reporter: [['list']],
  use: {
    ...devices['Desktop Chrome'],
    baseURL: process.env.DEMO_CONSOLE_URL ?? 'http://127.0.0.1:38001',
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: Number(process.env.SCREENSHOTS_SCALE ?? '1'),
    locale: 'en-US',
    timezoneId: 'UTC',
    colorScheme: 'light',
    contextOptions: { reducedMotion: 'reduce' },
    launchOptions: { executablePath: process.env.NEXO_E2E_CHROMIUM || undefined },
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
});
