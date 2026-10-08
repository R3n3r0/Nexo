import { defineConfig, devices } from '@playwright/test';

/**
 * End-to-end suite of the console (docs/06 §6.5, docs/09 §9.6): Chromium
 * against the real bin/nexo with the embedded dashboard. The global setup
 * builds the binary when it is stale, starts it on free ports with a
 * temporary drive and seeds the test identities; the teardown stops it by
 * PID. Run with `npm run e2e` (after `make build` in CI).
 */
export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  globalSetup: './e2e/global-setup.ts',
  globalTeardown: './e2e/global-teardown.ts',
  outputDir: './test-results',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: process.env.CI ? 2 : 4,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: process.env.CI
    ? [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]]
    : [['list']],
  use: {
    locale: 'en-US',
    timezoneId: 'UTC',
    acceptDownloads: true,
    // The console honours prefers-reduced-motion: no transition makes a
    // check (axe colour contrast, clicks) race an animation.
    contextOptions: { reducedMotion: 'reduce' },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 20_000,
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        launchOptions: {
          // The Chromium preinstalled in PLAYWRIGHT_BROWSERS_PATH (CI installs
          // the matching build with `npx playwright install chromium`).
          executablePath: process.env.NEXO_E2E_CHROMIUM || undefined,
        },
      },
    },
  ],
});
