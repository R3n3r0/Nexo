import AxeBuilder from '@axe-core/playwright';
import type { APIResponse, Browser, BrowserContext, Locator, Page } from '@playwright/test';

import { type NexoClient, uniqueKey, uniqueName } from './support/api';
import { contextAs, expect, test } from './support/fixtures';
import { type HostProxy, startHostProxy } from './support/hostProxy';

/**
 * The cache of the distributions from the console (docs/11 §11.9,
 * TD-C-01..06): the *.test domains reach the server under test through a
 * forward proxy keeping the domain in the Host header, as a DNS record
 * pointing them to the cluster would.
 */

interface Site {
  id: string;
  bucket: string;
  www: string;
  preview: string;
}

/** A bucket with a small site and a distribution serving it on two domains. */
async function createSite(api: NexoClient, prefix: string): Promise<Site> {
  const id = uniqueName(prefix);
  const bucket = uniqueName(`${prefix}-b`);
  const site = { id, bucket, www: `www.${id}.test`, preview: `preview.${id}.test` };
  await api.createBucket(bucket);
  await api.putObject(bucket, 'index.html', `<h1>Home of ${id}</h1>`, 'text/html');
  await api.putObject(bucket, 'assets/app.js', 'console.log("app");', 'text/javascript');
  await api.putObject(bucket, 'assets/app.css', 'body{}', 'text/css');
  await api.call('POST', '/distributions', {
    json: {
      id,
      enabled: true,
      aliases: [site.www, site.preview],
      origin: { bucket },
      mode: 'website',
      defaultBehavior: { access: 'public', indexDocument: 'index.html' },
    },
  });
  return site;
}

async function removeSite(api: NexoClient, site: Site | undefined) {
  if (site === undefined) return;
  await api.admin('DELETE', `/distributions/${site.id}`);
  await api.deleteBucket(site.bucket);
}

/** A browser context whose requests to the *.test domains reach the server. */
function visitor(browser: Browser, proxy: HostProxy): Promise<BrowserContext> {
  return browser.newContext({ proxy: { server: proxy.server } });
}

/** GET of a URL of a domain, with the X-Cache answer. */
async function fetchVia(context: BrowserContext, url: string) {
  const res: APIResponse = await context.request.get(url, { failOnStatusCode: false });
  return { status: res.status(), headers: res.headers(), body: await res.text() };
}

/**
 * Requests a URL until the cache answers a HIT (a node serves its entries
 * once it receives the invalidations of every peer: at once on a single
 * node).
 */
async function untilHit(context: BrowserContext, url: string) {
  await expect
    .poll(async () => (await fetchVia(context, url)).headers['x-cache'], { timeout: 20_000 })
    .toBe('HIT');
}

/** Opens the cache tab of a distribution at one of its sections. */
async function openCacheTab(page: Page, id: string, section?: string) {
  await page.goto(
    `/distributions/${id}/cache${section === undefined ? '' : `?section=${section}`}`,
  );
}

/** Saves the settings of the cache tab and waits for the server to keep them. */
async function saveCacheSettings(page: Page) {
  const bar = page.getByRole('region', { name: 'Unsaved cache settings' });
  await bar.getByRole('button', { name: 'Save' }).click();
  await expect(bar).toHaveCount(0);
}

/**
 * Clicks the label of a control whose input is visually hidden (Mantine
 * switches, segmented controls), as a pointer does.
 */
async function clickLabel(scope: Locator, text: string) {
  await scope.getByText(text, { exact: true }).click();
}

/** The serious and critical violations axe-core finds on the page. */
async function seriousViolations(page: Page) {
  const results = await new AxeBuilder({ page }).analyze();
  return results.violations
    .filter((v) => v.impact === 'serious' || v.impact === 'critical')
    .map((v) => ({
      rule: v.id,
      help: v.help,
      targets: v.nodes.slice(0, 5).map((n) => `${n.target.join(' ')} :: ${n.failureSummary ?? ''}`),
    }));
}

/** Waits until the page settled: no request in flight and no loader shown. */
async function settle(page: Page) {
  await page.waitForLoadState('networkidle');
  await expect(page.locator('.mantine-Loader-root')).toHaveCount(0);
  await expect(page.locator('.mantine-Skeleton-root')).toHaveCount(0);
}

test.describe('cache of the distributions', () => {
  let proxy: HostProxy;
  let site: Site | undefined;

  test.beforeAll(async ({ nexo }) => {
    proxy = await startHostProxy(nexo.s3URL);
  });

  test.afterAll(async () => {
    await proxy.close();
  });

  test.afterEach(async ({ api }) => {
    await removeSite(api, site);
    site = undefined;
  });

  // covers: D-CCH-02
  test('TD-C-01 a policy override of a domain in the cache tab changes the answers of that domain only', async ({
    page,
    api,
    browser,
  }) => {
    site = await createSite(api, 'e2e-c1');
    const { id, www, preview } = site;
    await openCacheTab(page, id);
    const domains = page.getByRole('table', { name: 'Domains' });
    const row = (host: string) => domains.locator(`tr[data-domain="${host}"]`);
    await expect(row(www)).toContainText('standard');

    // www: the immutable assets policy, from the domain level.
    await row(www)
      .getByRole('button', { name: `Override the cache of ${www}` })
      .click();
    let dialog = page.getByRole('dialog', { name: `Cache of ${www}` });
    await dialog.getByRole('textbox', { name: 'Policy' }).click();
    await page.getByRole('option', { name: 'immutable-assets', exact: true }).click();
    await dialog.getByRole('textbox', { name: 'Quota' }).fill('64MiB');
    await dialog.getByRole('button', { name: 'Apply' }).click();
    // preview: the cache off.
    await row(preview)
      .getByRole('button', { name: `Override the cache of ${preview}` })
      .click();
    dialog = page.getByRole('dialog', { name: `Cache of ${preview}` });
    await clickLabel(dialog, 'Disabled');
    await dialog.getByRole('button', { name: 'Apply' }).click();
    await expect(row(www)).toContainText('immutable-assets');
    await expect(row(preview).locator('[data-enabled]')).toHaveAttribute('data-enabled', 'false');
    await saveCacheSettings(page);

    const stored = await api.call<{ domains: { host: string; cache: object }[] }>(
      'GET',
      `/distributions/${id}/cache`,
    );
    expect(stored.domains).toEqual(
      expect.arrayContaining([
        { host: www, cache: { policy: 'immutable-assets', quota: '64MiB' } },
        { host: preview, cache: { enabled: false } },
      ]),
    );

    // The effect, seen by a browser whose DNS points the domains to the server.
    const context = await visitor(browser, proxy);
    try {
      const first = await fetchVia(context, `http://${www}/assets/app.js`);
      expect(first.status).toBe(200);
      expect(first.headers['cache-control']).toBe('public, max-age=31536000, immutable');
      expect(['MISS', 'HIT']).toContain(first.headers['x-cache']);
      await untilHit(context, `http://${www}/assets/app.js`);
      const other = await fetchVia(context, `http://${preview}/assets/app.js`);
      expect(other.status).toBe(200);
      expect(other.body).toBe('console.log("app");');
      expect(other.headers['x-cache']).toBe('BYPASS');
      expect(other.headers['cache-control']).not.toBe('public, max-age=31536000, immutable');
    } finally {
      await context.close();
    }
  });

  // covers: D-CCH-03
  test('TD-C-02 the policy editor previews the Cache-Control live and validates min ≤ default ≤ max', async ({
    page,
    api,
  }) => {
    const name = uniqueName('e2e-pol').toLowerCase();
    try {
      await page.goto('/cache/policies/-/new');
      const form = page.getByRole('form', { name: 'New cache policy' });
      await form.getByRole('textbox', { name: 'Name' }).fill(name);
      await form.getByRole('textbox', { name: 'TTL min' }).fill('2h');
      await expect(
        form.getByText('The minimum 2h is above the default 1h: min ≤ default ≤ max.'),
      ).toBeVisible();
      const create = form.getByRole('button', { name: 'Create policy' });
      await expect(create).toBeDisabled();
      await form.getByRole('textbox', { name: 'TTL max' }).fill('1h30m');
      await form.getByRole('textbox', { name: 'TTL default' }).fill('2h30m');
      await expect(
        form.getByText('The default 2h30m is above the maximum 1h30m: min ≤ default ≤ max.'),
      ).toBeVisible();
      await form.getByRole('textbox', { name: 'TTL max' }).fill('1d');
      await expect(create).toBeEnabled();

      const preview = page.getByRole('region', { name: 'Preview' });
      const header = preview.getByTestId('cache-control-preview');
      await expect(header).toContainText('the clients receive the Cache-Control of each object');
      await expect(preview).toContainText('Fresh in the cache for 2h30m (between 2h and 1d).');
      await clickLabel(form, 'Send a Cache-Control of the policy');
      await expect(header).toHaveText('public, max-age=3600');
      await form.getByRole('textbox', { name: 'max-age (seconds)' }).fill('600');
      await expect(header).toHaveText('public, max-age=600');
      await clickLabel(form, 'must-revalidate');
      await expect(header).toHaveText('public, max-age=600, must-revalidate');
      // The summary of the server (POST /cache/preview).
      await expect(preview.getByTestId('policy-summary')).toContainText('fresh 2h30m');

      await create.click();
      await expect(page).toHaveURL(new RegExp(`/cache/policies/${name}$`));
      const stored = await api.call<{ ttl: object; browserCacheControl: string }>(
        'GET',
        `/cache/policies/${name}`,
      );
      expect(stored.ttl).toEqual({ default: '2h30m0s', min: '2h0m0s', max: '1d' });
      expect(stored.browserCacheControl).toBe('public, max-age=600, must-revalidate');
      // The server refuses an inverted TTL too.
      const res = await api.admin('PUT', `/cache/policies/${name}`, {
        json: { name, ttl: { default: '1h', max: '1m' } },
      });
      expect(res.status).toBe(400);
    } finally {
      await api.admin('DELETE', `/cache/policies/${name}`);
    }
  });

  // covers: D-CCH-04, D-DST-08
  test('TD-C-03 an invalidation by glob from the dashboard completes on every node', async ({
    page,
    api,
    browser,
    nexo,
  }) => {
    site = await createSite(api, 'e2e-c3');
    const { id, www } = site;
    const context = await visitor(browser, proxy);
    try {
      for (const path of ['/assets/app.js', '/index.html']) {
        await fetchVia(context, `http://${www}${path}`);
        await untilHit(context, `http://${www}${path}`);
      }

      await openCacheTab(page, id, 'invalidations');
      const form = page.getByRole('form', { name: 'Invalidate' });
      await clickLabel(form, 'Patterns');
      await form.getByRole('textbox', { name: 'Pattern 1' }).fill('/assets/**');
      await form.getByRole('textbox', { name: 'Only on the domain' }).click();
      await page.getByRole('option', { name: www, exact: true }).click();
      await form.getByRole('button', { name: 'Invalidate' }).click();

      const progress = page.getByRole('region', { name: /^Invalidation inv-/ });
      await expect(progress).toContainText(`domain ${www} · glob /assets/**`);
      const nodes = (nexo.nodes ?? [nexo]).length;
      await expect(
        progress.getByLabel(`Completed: ${String(nodes)} of ${String(nodes)} nodes`),
      ).toBeVisible({ timeout: 30_000 });
      await expect(progress.getByRole('table').getByText('Completed')).toHaveCount(nodes);
      const history = page.getByRole('table', { name: `History of ${id}` });
      await expect(history).toContainText('glob /assets/**');

      // The assets of the domain were dropped; the rest stays cached.
      expect((await fetchVia(context, `http://${www}/assets/app.js`)).headers['x-cache']).toBe(
        'MISS',
      );
      expect((await fetchVia(context, `http://${www}/index.html`)).headers['x-cache']).toBe('HIT');
    } finally {
      await context.close();
    }
  });

  // covers: D-CCH-05
  test('TD-C-04 the URL inspector shows the effective policy and the level it comes from', async ({
    page,
    api,
    browser,
  }) => {
    site = await createSite(api, 'e2e-c4');
    const { id, www, preview } = site;
    await api.call('PUT', `/distributions/${id}/cache`, {
      json: {
        cache: { policy: 'standard' },
        domains: [
          { host: www, cache: { policy: 'aggressive' } },
          { host: preview, cache: { enabled: false } },
        ],
        behaviors: [
          {
            pathPattern: { type: 'glob', pattern: '/assets/**' },
            cache: { ttl: { default: '365d', max: '365d' } },
          },
        ],
      },
    });
    const context = await visitor(browser, proxy);
    try {
      await fetchVia(context, `http://${www}/assets/app.js`);
      await untilHit(context, `http://${www}/assets/app.js`);
    } finally {
      await context.close();
    }

    await page.goto('/cache/inspector');
    const form = page.getByRole('form', { name: 'URL inspector' });
    await form.getByRole('textbox', { name: 'URL' }).fill(`http://${www}/assets/app.js`);
    await form.getByRole('button', { name: 'Inspect' }).click();
    const result = page.getByRole('region', { name: 'How the cache serves the URL' });
    const policy = result.getByRole('table', { name: 'Effective policy of the URL' });
    const row = (name: string) => policy.getByRole('row', { name: new RegExp(`^${name} `) });
    await expect(row('Cache')).toContainText('Enabled');
    await expect(row('Policy')).toContainText('aggressive');
    await expect(row('Policy').locator('[data-level]')).toHaveAttribute('data-level', 'domain');
    await expect(row('Lifetime in cache')).toContainText('365d');
    await expect(row('Lifetime in cache').locator('[data-level]')).toHaveAttribute(
      'data-level',
      'behavior',
    );
    await expect(result).toContainText(`${site.bucket}/assets/app.js`);
    const nodes = result.getByRole('table', { name: 'Entry of the URL on every node' });
    await expect(nodes.locator('[data-present="true"]')).not.toHaveCount(0);

    // Every setting with its level, on demand.
    await result.getByRole('button', { name: 'Every setting and where it comes from' }).click();
    const settings = result.getByRole('table', { name: 'Settings of the effective policy' });
    await expect(
      settings.getByRole('row', { name: /^staleIfError 7d/ }).locator('[data-level]'),
    ).toHaveAttribute('data-level', 'domain');

    // The preview domain: the cache disabled by its domain settings.
    await form.getByRole('textbox', { name: 'URL' }).fill(`http://${preview}/index.html`);
    await form.getByRole('button', { name: 'Inspect' }).click();
    await expect(result.getByText('The cache is bypassed')).toBeVisible();
    await expect(row('Cache')).toContainText('Disabled');
    await expect(row('Cache').locator('[data-level]')).toHaveAttribute('data-level', 'domain');
  });

  // covers: D-CCH-08
  test('TD-C-05 the hit ratio charts per domain show the requests of the test', async ({
    page,
    api,
    browser,
  }) => {
    site = await createSite(api, 'e2e-c5');
    const { id, www, preview } = site;
    await api.call('PUT', `/distributions/${id}/cache`, {
      json: { cache: {}, domains: [{ host: preview, cache: { enabled: false } }] },
    });
    const context = await visitor(browser, proxy);
    try {
      // www: one MISS, then three HITs (75%); preview: bypassed.
      await fetchVia(context, `http://${www}/index.html`);
      await untilHit(context, `http://${www}/index.html`);
      let hits = 1;
      while (hits < 3) {
        expect((await fetchVia(context, `http://${www}/index.html`)).headers['x-cache']).toBe(
          'HIT',
        );
        hits += 1;
      }
      for (let i = 0; i < 2; i += 1) {
        expect((await fetchVia(context, `http://${preview}/index.html`)).headers['x-cache']).toBe(
          'BYPASS',
        );
      }
    } finally {
      await context.close();
    }
    const stats = await api.call<{ hosts: { host: string; hitRatio: number }[] }>(
      'GET',
      '/cache/stats',
      { query: { distribution: id } },
    );
    const ratio = stats.hosts.find((h) => h.host === www)?.hitRatio ?? 0;
    expect(ratio).toBeGreaterThan(0.5);
    const percent = `${String(Math.round(ratio * 1000) / 10)}%`;

    await openCacheTab(page, id, 'statistics');
    const domains = page.getByRole('table', { name: 'Domains' });
    await expect(domains.locator(`tr[data-host="${www}"] [data-column="hit-ratio"]`)).toHaveText(
      percent,
    );
    await expect(
      domains.locator(`tr[data-host="${preview}"] [data-column="hit-ratio"]`),
    ).toHaveText('—');
    // The charts, drawn with these figures: one bar per domain with requests.
    const ratioChart = page.locator('figure').filter({ hasText: 'Hit ratio per domain' });
    await expect(ratioChart.getByRole('img', { name: 'Hit ratio per domain' })).toBeVisible();
    // The preview domain is bypassed: no hit ratio, no bar.
    await expect(ratioChart.locator('.recharts-bar-rectangle')).toHaveCount(1);
    const answers = page.locator('figure').filter({ hasText: 'Answers per domain by X-Cache' });
    await expect(answers.getByText('BYPASS', { exact: true }).first()).toBeVisible();
    await expect(answers.getByText('HIT', { exact: true }).first()).toBeVisible();
    // The global page shows the domain among the top domains.
    await page.goto('/cache/overview');
    const top = page.getByRole('table', { name: 'Top domains' });
    await expect(top.locator(`tr[data-host="${www}"] [data-column="hit-ratio"]`)).toHaveText(
      percent,
    );
  });

  // covers: D-CCH-04, D-UX-02
  test('TD-C-06 a user with admin:InvalidateCache alone invalidates but cannot change the policies', async ({
    api,
    browser,
    nexo,
  }) => {
    site = await createSite(api, 'e2e-c6');
    const { id } = site;
    const policy = uniqueName('e2e-purge');
    const user = { accessKey: uniqueKey('deploy'), secretKey: 'deploy-secret-123' };
    await api.call('PUT', `/policies/${policy}`, {
      json: {
        document: {
          Version: '2012-10-17',
          Statement: [
            { Effect: 'Allow', Action: ['admin:InvalidateCache'], Resource: ['arn:aws:s3:::*'] },
          ],
        },
      },
    });
    await api.call('POST', '/users', { json: { ...user, policyNames: [policy] } });
    const context = await contextAs(browser, nexo, user);
    try {
      const page = await context.newPage();
      await page.goto('/cache');
      await expect(page).toHaveURL(/\/cache\/invalidations$/);
      const main = page.getByRole('navigation', { name: 'Main navigation' });
      await expect(main.getByRole('link', { name: 'Cache' })).toBeVisible();
      await expect(main.getByRole('link', { name: 'Distributions' })).toHaveCount(0);
      const sections = page.getByRole('navigation', { name: 'Cache sections' });
      await expect(sections.getByRole('link')).toHaveText(['Invalidations', 'Warm-up']);

      const form = page.getByRole('form', { name: 'Invalidate' });
      await form.getByRole('textbox', { name: 'Distribution' }).fill(id);
      await form.getByRole('textbox', { name: 'Paths' }).fill('/index.html');
      await form.getByRole('button', { name: 'Invalidate' }).click();
      const progress = page.getByRole('region', { name: /^Invalidation inv-/ });
      await expect(progress.getByLabel(/^Completed: /)).toBeVisible({ timeout: 30_000 });
      await expect(page.getByRole('table', { name: `History of ${id}` })).toContainText(
        user.accessKey,
      );

      await page.goto('/cache/policies');
      await expect(
        page.getByText('You may not open Policies: admin:ManageCache is required.'),
      ).toBeVisible();
      await page.goto('/cache/policies/-/new');
      await expect(
        page.getByText('You do not have the permission admin:ManageCache.'),
      ).toBeVisible();
      await expect(page.getByRole('form', { name: 'New cache policy' })).toHaveCount(0);

      // The server refuses the configuration whatever the console shows.
      const deployer = api.as(user);
      expect(
        (
          await deployer.admin('POST', '/cache/policies', {
            json: { name: 'nope', ttl: { default: '1h' } },
          })
        ).status,
      ).toBe(403);
      expect(
        (await deployer.admin('PUT', `/distributions/${id}/cache`, { json: { cache: {} } })).status,
      ).toBe(403);
      expect((await deployer.admin('GET', '/cache/stats')).status).toBe(403);
      const purge = await deployer.admin('POST', `/distributions/${id}/invalidations`, {
        json: { paths: ['/'] },
      });
      expect(purge.ok, String(purge.status)).toBe(true);
    } finally {
      await context.close();
      await api.admin('DELETE', `/users/${user.accessKey}`);
      await api.admin('DELETE', `/policies/${policy}`);
    }
  });

  // covers: D-UX-01
  test('TD-E-19 the cache pages have no serious or critical axe violation', async ({
    page,
    api,
    browser,
  }) => {
    site = await createSite(api, 'e2e-ca11y');
    const { id, www } = site;
    const context = await visitor(browser, proxy);
    try {
      await fetchVia(context, `http://${www}/index.html`);
      await fetchVia(context, `http://${www}/index.html`);
    } finally {
      await context.close();
    }
    const check = async (label: string) => {
      await settle(page);
      expect(await seriousViolations(page), label).toEqual([]);
    };

    await page.goto('/cache/overview');
    await expect(page.getByRole('table', { name: 'Top domains' })).toBeVisible();
    await check('overview');
    await page.goto('/cache/policies');
    await expect(page.getByRole('table', { name: 'Cache policies' })).toBeVisible();
    await check('policies');
    await page.goto('/cache/policies/-/new');
    await page.getByRole('textbox', { name: 'TTL min' }).fill('3h');
    await expect(page.getByText(/The minimum 3h is above the default/)).toBeVisible();
    await check('policy editor with a problem');
    await page.goto('/cache/inspector');
    await page.getByRole('textbox', { name: 'URL' }).fill(`http://${www}/index.html`);
    await page.getByRole('button', { name: 'Inspect' }).click();
    await expect(page.getByRole('region', { name: 'How the cache serves the URL' })).toBeVisible();
    await check('inspector');
    await page.goto('/cache/settings');
    await expect(page.getByRole('region', { name: 'Settings of the cluster' })).toBeVisible();
    await check('settings');
    await openCacheTab(page, id);
    await expect(page.getByRole('table', { name: 'Domains' })).toBeVisible();
    await check('cache tab');
    await page.getByRole('button', { name: `Override the cache of ${www}` }).click();
    await expect(page.getByRole('dialog', { name: `Cache of ${www}` })).toBeVisible();
    await check('domain override');
    await page.keyboard.press('Escape');
    await openCacheTab(page, id, 'webhooks');
    await expect(page.getByRole('region', { name: 'Purge webhooks' })).toBeVisible();
    await check('webhooks');
    await openCacheTab(page, id, 'invalidations');
    const form = page.getByRole('form', { name: 'Invalidate' });
    await form.getByRole('textbox', { name: 'Paths' }).fill('/index.html');
    await form.getByRole('button', { name: 'Invalidate' }).click();
    await expect(page.getByRole('region', { name: /^Invalidation inv-/ })).toBeVisible();
    await check('invalidation');
  });
});
