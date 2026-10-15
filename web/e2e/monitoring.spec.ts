import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';

import AxeBuilder from '@axe-core/playwright';
import type { Page } from '@playwright/test';

import { uniqueKey, uniqueName } from './support/api';
import { Nexoctl } from './support/cli';
import { contextAs, expect, test } from './support/fixtures';
import { rootCredentials } from './support/nexo';

/**
 * Monitoring (T-8.6, D-MON-01..03, D-CFG-01): the charts of the metrics,
 * the live trace while the CLI works (TD-E-16), the live logs, the search
 * of the audit log, the observability settings and the permissions that
 * gate them, with axe on every section (TD-E-19).
 */

/** The serious and critical violations of axe-core on the page. */
async function seriousViolations(page: Page) {
  const results = await new AxeBuilder({ page }).analyze();
  return results.violations
    .filter((v) => v.impact === 'serious' || v.impact === 'critical')
    .map((v) => ({
      rule: v.id,
      impact: v.impact,
      help: v.help,
      targets: v.nodes.slice(0, 5).map((n) => `${n.target.join(' ')} :: ${n.failureSummary ?? ''}`),
    }));
}

/**
 * Waits until no loader nor skeleton is shown. The live sections keep a
 * stream open, so "networkidle" never comes: the state of the stream says
 * when they are ready.
 */
async function settled(page: Page) {
  await expect(page.locator('.mantine-Loader-root')).toHaveCount(0);
  await expect(page.locator('.mantine-Skeleton-root')).toHaveCount(0);
}

/** Waits for the live stream of the section to be open. */
async function live(page: Page) {
  await expect(page.getByRole('status', { name: 'Stream status: Live' })).toBeVisible({
    timeout: 20_000,
  });
}

// covers: D-MON-01
test.describe('monitoring (D-MON-01..03)', () => {
  let bucket: string;
  let file: string;

  test.beforeAll(async ({ api, nexo }) => {
    bucket = uniqueName('e2e-mon');
    await api.createBucket(bucket);
    file = join(nexo.workDir, `${bucket}.txt`);
    await writeFile(file, 'traced by the console');
  });

  test.afterAll(async ({ api }) => {
    await api.deleteBucket(bucket);
  });

  // covers: D-MON-02
  test('TD-E-16 the live trace shows the requests of the CLI as they happen', async ({
    page,
    nexo,
  }) => {
    const cli = new Nexoctl(nexo);
    await page.goto('/monitoring/trace');
    const filters = page.getByRole('region', { name: 'Filters of the trace' });
    const buckets = filters.getByRole('textbox', { name: 'Buckets' });
    await buckets.fill(bucket);
    await buckets.press('Enter');
    await live(page);

    const table = page.getByRole('table', { name: 'Live requests' });
    const row = (key: string) =>
      table
        .getByRole('row')
        .filter({ hasText: 'PutObject' })
        .filter({ hasText: `/${bucket}/${key}` });

    await cli.ok('cp', file, `e2e/${bucket}/first.txt`);
    await expect(row('first.txt')).toBeVisible();
    await expect(row('first.txt')).toContainText('200');
    // Only the bucket of the filter: the other requests of the suite are
    // not shown.
    for (const text of await table.getByRole('row').allTextContents()) {
      if (text.includes('Request')) continue; // the header
      expect(text).toContain(`/${bucket}`);
    }

    // Paused, the requests are held, then shown on resume.
    await page.getByRole('button', { name: 'Pause' }).click();
    await expect(page.getByRole('status', { name: 'Stream status: Paused' })).toBeVisible();
    await cli.ok('cp', file, `e2e/${bucket}/while-paused.txt`);
    await expect(page.getByText(/\d+ new while paused/)).toBeVisible();
    await expect(row('while-paused.txt')).toHaveCount(0);
    await page.getByRole('button', { name: 'Resume' }).click();
    await expect(row('while-paused.txt')).toBeVisible();

    // The details of a request, without secrets.
    await row('first.txt')
      .getByRole('button', { name: /^Details of PutObject at / })
      .click();
    const details = page.getByRole('region', { name: 'Details' });
    await expect(details).toContainText(`PUT /${bucket}/first.txt`);
    await expect(details).toContainText('Request ID');
    await expect(details).not.toContainText(rootCredentials.secretKey);
    await settled(page);
    expect(await seriousViolations(page), 'trace with requests and details').toEqual([]);

    // A stricter filter reopens the stream: errors only hides the uploads.
    await filters.locator('label', { hasText: /^Errors only$/ }).click();
    await expect(filters.getByRole('switch', { name: 'Errors only' })).toBeChecked();
    await live(page);
    await cli.ok('cp', file, `e2e/${bucket}/hidden.txt`);
    const missing = await cli.run('stat', `e2e/${bucket}/missing.txt`);
    expect(missing.code).toBe(3);
    await expect(
      table
        .getByRole('row')
        .filter({ hasText: `/${bucket}/missing.txt` })
        .first(),
    ).toBeVisible();
    await expect(row('hidden.txt')).toHaveCount(0);
  });

  test('the live logs replay the recent entries and filter them', async ({ page }) => {
    await page.goto('/monitoring/logs');
    await live(page);
    const table = page.getByRole('table', { name: 'Live log entries' });
    // The replay of the recent entries of the node (the audit records and
    // the start of the server are logged at INFO).
    await expect(table.getByRole('row').nth(1)).toBeVisible();
    await expect(table).not.toContainText('Waiting for log entries');
    await settled(page);
    expect(await seriousViolations(page), 'logs').toEqual([]);

    const filters = page.getByRole('region', { name: 'Filters of the logs' });
    await filters.getByRole('searchbox', { name: 'Search the lines' }).fill('no-such-line-xyz');
    await expect(table).toContainText('No line kept matches the search.');
    await filters.getByRole('searchbox', { name: 'Search the lines' }).fill('');

    // The radios of a segmented control are visually hidden: their labels take the clicks.
    await filters.locator('label', { hasText: /^Error$/ }).click();
    await expect(filters.getByRole('radio', { name: 'Error' })).toBeChecked();
    await live(page);
    for (const level of await table.getByRole('row').locator('td:nth-child(3)').allTextContents()) {
      expect(level).toBe('ERROR');
    }
  });

  test('the audit log finds the records of a user, an action and a bucket', async ({
    page,
    api,
  }) => {
    await api.putObject(bucket, 'audited.txt', 'audited', 'text/plain');
    await page.goto('/monitoring/audit');
    const form = page.getByRole('region', { name: 'Search the audit log' });
    await form.getByRole('textbox', { name: 'Action' }).fill('PutObject');
    await form.getByRole('textbox', { name: 'Bucket' }).fill(bucket);
    const results = page.getByRole('region', { name: 'Audit records' });
    const record = results.getByRole('row').filter({ hasText: `${bucket}/audited.txt` });
    await expect(async () => {
      await form.getByRole('button', { name: 'Search' }).click();
      await expect(record).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 20_000 });
    await expect(record).toContainText('PutObject');
    await expect(record).toContainText('200');
    await expect(results.getByRole('status')).toContainText(/record/);

    await record.getByRole('button', { name: /^Details of the record / }).click();
    await expect(results).toContainText('Method and path');
    await expect(results).toContainText(`PUT /${bucket}/audited.txt`);
    await settled(page);
    expect(await seriousViolations(page), 'audit with results').toEqual([]);

    // The outcome narrows the records: no denied upload here.
    await form.getByRole('textbox', { name: 'Outcome' }).click();
    await page.getByRole('option', { name: 'Denied (403)' }).click();
    await form.getByRole('button', { name: 'Search' }).click();
    await expect(results).toContainText('No audit record matches the search.');
  });

  test('the charts of the metrics, over a time range, refreshed', async ({ page, api }) => {
    await api.putObject(bucket, 'metric.txt', 'metric', 'text/plain');
    await page.goto('/monitoring/metrics');
    const requests = page.getByRole('region', { name: 'Requests' });
    for (const title of [
      'Requests',
      'Errors',
      'S3 latency',
      'Bandwidth',
      'Disk usage',
      'Drives',
      'Healing and scanner',
      'CPU',
      'Memory',
    ]) {
      await expect(page.getByRole('region', { name: title })).toBeVisible();
    }
    // The server samples every 10 s: refresh until the first points.
    await expect(async () => {
      await page.getByRole('button', { name: 'Refresh' }).click();
      await expect(requests.getByRole('img', { name: 'Requests' })).toBeVisible({
        timeout: 2_000,
      });
    }).toPass({ timeout: 45_000 });
    await expect(requests).toContainText(/Latest: S3 .* · Every listener /);
    await expect(
      page.getByRole('region', { name: 'Disk usage' }).getByText(/Latest: Used .*B · Free .*B/),
    ).toBeVisible();

    await page.locator('label', { hasText: /^6 hours$/ }).click();
    await expect(page.getByRole('radio', { name: '6 hours' })).toBeChecked();
    await expect(page).toHaveURL(/\/monitoring\/metrics\?range=6h$/);
    await expect(requests.getByRole('img', { name: 'Requests' })).toBeVisible();
    await page.getByRole('textbox', { name: 'Auto-refresh' }).click();
    await page.getByRole('option', { name: 'Every 10 s' }).click();
    await expect(page.getByText(/^Updated /)).toBeVisible();
    await settled(page);
    expect(await seriousViolations(page), 'metrics with charts').toEqual([]);
  });

  test('the observability settings and the state of TLS and tracing', async ({ page }) => {
    await page.goto('/configuration');
    const section = page.getByRole('region', { name: 'Observability' });
    await expect(section.getByRole('table', { name: 'Audit targets' })).toBeVisible();
    await expect(section).toContainText('Distributed tracing (OpenTelemetry)');
    await expect(section).toContainText('Disabled: the listeners serve plain HTTP');
    await section.getByRole('link', { name: 'Open the live logs' }).click();
    await expect(page).toHaveURL(/\/monitoring\/logs$/);

    await page.goto('/info');
    await expect(page.getByRole('region', { name: 'TLS' })).toContainText(
      'Disabled: the listeners serve plain HTTP',
    );
    await expect(
      page.getByRole('region', { name: 'Distributed tracing (OpenTelemetry)' }),
    ).toContainText('Disabled');
  });
});

// covers: D-UX-02
test.describe('monitoring permissions (D-UX-02)', () => {
  test('the sections follow admin:Prometheus, admin:ServerTrace and admin:ConsoleLog', async ({
    browser,
    api,
    nexo,
  }) => {
    const diag = { accessKey: uniqueKey('e2e-diag'), secretKey: 'e2e-diag-secret-0123' };
    const tracer = { accessKey: uniqueKey('e2e-tracer'), secretKey: 'e2e-tracer-secret-01' };
    const policy = uniqueName('e2e-trace-only');
    await api.call('POST', '/users', { json: { ...diag, policyNames: ['diagnostics'] } });
    await api.call('POST', '/policies', {
      json: {
        name: policy,
        document: {
          Version: '2012-10-17',
          Statement: [
            { Effect: 'Allow', Action: ['admin:ServerTrace'], Resource: ['arn:aws:s3:::*'] },
          ],
        },
      },
    });
    await api.call('POST', '/users', { json: { ...tracer, policyNames: [policy] } });
    try {
      // The diagnostics policy opens every section, not the configuration.
      const diagContext = await contextAs(browser, nexo, diag);
      try {
        const page = await diagContext.newPage();
        await page.goto('/monitoring');
        await expect(page).toHaveURL(/\/monitoring\/metrics$/);
        const nav = page.getByRole('navigation', { name: 'Main navigation' });
        await expect(nav.getByRole('link', { name: 'Monitoring' })).toBeVisible();
        await expect(nav.getByRole('link', { name: 'Configuration' })).toHaveCount(0);
        const sections = page.getByRole('navigation', { name: 'Monitoring sections' });
        await expect(sections.getByRole('link')).toHaveText([
          'Metrics',
          'Live trace',
          'Live logs',
          'Audit',
        ]);
        await sections.getByRole('link', { name: 'Live logs' }).click();
        await live(page);
      } finally {
        await diagContext.close();
      }

      // admin:ServerTrace alone: the trace only.
      const tracerContext = await contextAs(browser, nexo, tracer);
      try {
        const page = await tracerContext.newPage();
        await page.goto('/monitoring');
        await expect(page).toHaveURL(/\/monitoring\/trace$/);
        await live(page);
        const sections = page.getByRole('navigation', { name: 'Monitoring sections' });
        await expect(sections.getByRole('link')).toHaveText(['Live trace']);
        await page.goto('/monitoring/audit');
        await expect(page.getByText('Your policies do not allow the section Audit.')).toBeVisible();
        // The server refuses the logs whatever the console shows.
        const res = await page.request.get('/api/v1/logs');
        expect(res.status()).toBe(403);
      } finally {
        await tracerContext.close();
      }

      // No monitoring permission: no menu, an explanation.
      const memberContext = await contextAs(browser, nexo, nexo.users.member);
      try {
        const page = await memberContext.newPage();
        await page.goto('/buckets');
        const nav = page.getByRole('navigation', { name: 'Main navigation' });
        await expect(nav.getByRole('link', { name: 'Buckets' })).toBeVisible();
        await expect(nav.getByRole('link', { name: 'Monitoring' })).toHaveCount(0);
        await page.goto('/monitoring');
        await expect(page.getByRole('alert')).toContainText(
          'Your policies grant none of the monitoring permissions',
        );
        expect((await page.request.get('/api/v1/trace')).status()).toBe(403);
      } finally {
        await memberContext.close();
      }
    } finally {
      await api.call('DELETE', `/users/${diag.accessKey}`);
      await api.call('DELETE', `/users/${tracer.accessKey}`);
      await api.call('DELETE', `/policies/${policy}`);
    }
  });
});
