import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { defineConfig, devices } from '@playwright/test';

/**
 * End-to-end suite of the console (docs/06 §6.5, docs/09 §9.6): Chromium
 * against the real bin/nexo with the embedded dashboard, in the deployment
 * modes of docs/08 §8.6 (TM-02):
 *
 * - "chromium": the whole suite against a single drive (SNSD); the global
 *   setup builds the binary when it is stale, starts it on free ports with
 *   a temporary drive and seeds the test identities, the global teardown
 *   stops it by PID;
 * - "snmd": TD-E-01..04 against one node with 4 drives, started by the
 *   setup project "snmd-setup" and stopped by "snmd-teardown";
 * - "cluster": TD-E-15 against a cluster of 4 bin/nexo processes of 2
 *   drives each, started by "cluster-setup" and stopped by
 *   "cluster-teardown", after "cluster-smoke": TD-E-01..04 against the
 *   same cluster (its scenario stops a node: the smoke runs first);
 * - "cluster-full" (with NEXO_E2E_CLUSTER_FULL=1, the nightly CI job):
 *   the rest of the suite of the single drive on the same cluster, before
 *   the scenario of "cluster" (TM-02).
 *
 * Run with `npm run e2e` (after `make build` in CI), one mode with
 * `--project chromium|snmd|cluster`.
 */

// The state files of the mode projects live in a directory of this run,
// shared with the workers through the environment.
process.env.NEXO_E2E_RUN_DIR ??= join(tmpdir(), `nexo-e2e-run-${String(process.pid)}`);
const runDir = process.env.NEXO_E2E_RUN_DIR;

const chrome = {
  ...devices['Desktop Chrome'],
  launchOptions: {
    // The Chromium preinstalled in PLAYWRIGHT_BROWSERS_PATH (CI installs
    // the matching build with `npx playwright install chromium`).
    executablePath: process.env.NEXO_E2E_CHROMIUM || undefined,
  },
};

// The whole suite on the cluster is the nightly run (TM-02): it doubles
// the duration of the suite.
const clusterFull = !!process.env.NEXO_E2E_CLUSTER_FULL;

/** Options of the fixtures of e2e/support/fixtures.ts set by the projects. */
interface NexoOptions {
  nexoStateFile: string | undefined;
}

export default defineConfig<object, NexoOptions>({
  testDir: './e2e',
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
      testMatch: /e2e\/[^/]+\.spec\.ts$/,
      use: chrome,
    },
    {
      name: 'snmd-setup',
      testMatch: /e2e\/modes\/snmd\.setup\.ts$/,
      teardown: 'snmd-teardown',
    },
    { name: 'snmd-teardown', testMatch: /e2e\/modes\/snmd\.teardown\.ts$/ },
    {
      name: 'snmd',
      // TD-E-01..04 of the single-node smoke, in SNMD.
      testMatch: /e2e\/(auth|navigation|buckets)\.spec\.ts$/,
      grep: /TD-E-0[1-4]\b/,
      dependencies: ['snmd-setup'],
      use: { ...chrome, nexoStateFile: join(runDir, 'snmd-state.json') },
    },
    {
      name: 'cluster-setup',
      testMatch: /e2e\/cluster\/cluster\.setup\.ts$/,
      teardown: 'cluster-teardown',
    },
    { name: 'cluster-teardown', testMatch: /e2e\/cluster\/cluster\.teardown\.ts$/ },
    {
      name: 'cluster-smoke',
      // TD-E-01..04 of the single-node smoke, on the cluster (TM-02).
      testMatch: /e2e\/(auth|navigation|buckets)\.spec\.ts$/,
      grep: /TD-E-0[1-4]\b/,
      dependencies: ['cluster-setup'],
      use: { ...chrome, nexoStateFile: join(runDir, 'distributed-state.json') },
    },
    ...(clusterFull
      ? [
          {
            name: 'cluster-full',
            testMatch: /e2e\/[^/]+\.spec\.ts$/,
            // Excluded: TD-E-01..04, run by "cluster-smoke" above, and
            // TD-S-02, which starts a single-drive TLS server of its own
            // (it does not depend on the deployment under test).
            grepInvert: /TD-E-0[1-4]\b|TD-S-02\b/,
            dependencies: ['cluster-smoke'],
            use: { ...chrome, nexoStateFile: join(runDir, 'distributed-state.json') },
          },
        ]
      : []),
    {
      name: 'cluster',
      testMatch: /e2e\/cluster\/[^/]+\.spec\.ts$/,
      dependencies: [clusterFull ? 'cluster-full' : 'cluster-smoke'],
      // The scenario stops and resumes a node: one test at a time.
      fullyParallel: false,
      timeout: 240_000,
      use: { ...chrome, nexoStateFile: join(runDir, 'distributed-state.json') },
    },
  ],
});
