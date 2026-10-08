import {
  test as base,
  expect,
  type Browser,
  type BrowserContext,
  type Page,
} from '@playwright/test';

import { NexoClient } from './api';
import { type E2EState, readState } from './env';
import type { Credentials } from './sigv4';

export { expect };

interface TestFixtures {
  /** Root client of the Admin API and of the S3 API (SigV4). */
  api: NexoClient;
  /** Uncaught page errors, checked after every test. */
  pageErrors: string[];
}

interface WorkerFixtures {
  nexo: E2EState;
}

/**
 * Base test of the suite: the pages open on the console of the server
 * started by the global setup, signed in as root (storage state) unless a
 * spec opts out with test.use({ storageState: anonymous }).
 */
export const test = base.extend<TestFixtures, WorkerFixtures>({
  nexo: [
    // eslint-disable-next-line no-empty-pattern
    async ({}, use) => {
      await use(readState());
    },
    { scope: 'worker' },
  ],
  baseURL: async ({ nexo }, use) => {
    await use(nexo.consoleURL);
  },
  storageState: async ({ nexo }, use) => {
    await use(nexo.rootStorageState);
  },
  api: async ({ nexo }, use) => {
    await use(new NexoClient(nexo));
  },
  pageErrors: [
    async ({ page }, use) => {
      const errors: string[] = [];
      page.on('pageerror', (error) => {
        errors.push(error.stack ?? error.message);
      });
      await use(errors);
      expect(errors, 'uncaught errors in the page').toEqual([]);
    },
    { auto: true },
  ],
});

/** Storage state without any session. */
export const anonymous = { cookies: [], origins: [] };

/** Signs in through the login form and waits for the console shell. */
export async function loginWithForm(page: Page, creds: Credentials): Promise<void> {
  await page.getByRole('textbox', { name: 'Access key' }).fill(creds.accessKey);
  await page.getByRole('textbox', { name: 'Secret key' }).fill(creds.secretKey);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: `Signed in as ${creds.accessKey}` })).toBeVisible();
}

/** A new browser context signed in (through the Admin API) as creds. */
export async function contextAs(
  browser: Browser,
  nexo: E2EState,
  creds: Credentials,
): Promise<BrowserContext> {
  const context = await browser.newContext({ baseURL: nexo.consoleURL });
  const res = await context.request.post('/api/v1/auth/login', {
    data: { accessKey: creds.accessKey, secretKey: creds.secretKey },
  });
  expect(res.status(), await res.text()).toBe(200);
  return context;
}
