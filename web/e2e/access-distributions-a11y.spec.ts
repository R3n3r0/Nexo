import AxeBuilder from '@axe-core/playwright';
import type { Page } from '@playwright/test';

import { uniqueName } from './support/api';
import { selfSigned } from './support/certs';
import { expect, test } from './support/fixtures';

/** The serious and critical violations axe-core finds on the page. */
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

/** Waits until the page settled: no request in flight and no loader shown. */
async function settle(page: Page) {
  await page.waitForLoadState('networkidle');
  await expect(page.locator('.mantine-Loader-root')).toHaveCount(0);
  await expect(page.locator('.mantine-Skeleton-root')).toHaveCount(0);
}

/**
 * TD-E-19 on the pages of the access rules and of the distributions
 * (D-ACC-01..04, D-DST-01..08): no serious or critical axe violation, in
 * their states with content, results and dialogs.
 */
test.describe('accessibility of the access rules and distributions (axe-core)', () => {
  let bucket: string;
  let distribution: string;
  let group: string;
  let certificate: string;

  test.beforeAll(async ({ api, nexo }) => {
    bucket = uniqueName('e2e-a11y-acc');
    distribution = uniqueName('e2e-a11y-dist');
    group = uniqueName('e2e-a11y-kg');
    await api.createBucket(bucket);
    await api.putObject(bucket, 'img/2026/a.png', Buffer.from('png'), 'image/png');
    await api.putObject(bucket, 'docs/private/b.txt', 'b', 'text/plain');
    await api.call('PUT', `/buckets/${bucket}/access-rules`, {
      json: {
        rules: [
          {
            id: 'img-pub',
            priority: 10,
            effect: 'allow',
            principals: ['*'],
            actions: ['read'],
            match: { type: 'regex', pattern: '^img/[0-9]{4}/.*\\.png$' },
          },
          {
            id: 'no-private',
            priority: 5,
            effect: 'deny',
            principals: ['*'],
            actions: ['all'],
            match: { type: 'glob', pattern: '**/private/**' },
            conditions: { sourceIp: ['10.0.0.0/8'] },
          },
        ],
      },
    });
    // An expiring certificate: the highlighted state of the lists.
    certificate = `${distribution}.test`;
    await api.call('POST', '/certificates', {
      json: await selfSigned(nexo.workDir, certificate, 10),
    });
    await api.call('POST', '/keygroups', {
      json: { id: group, generateKey: { algorithm: 'ed25519' } },
    });
    await api.call('POST', '/distributions', {
      json: {
        id: distribution,
        enabled: true,
        aliases: [`${distribution}.test`, `*.${distribution}.test`],
        origin: { bucket, pathPrefix: '/img' },
        mode: 'website',
        defaultBehavior: { access: 'public', indexDocument: 'index.html' },
        behaviors: [
          {
            pathPattern: { type: 'glob', pattern: '/2026/**' },
            cache: { cacheControl: 'public, max-age=60' },
          },
          {
            pathPattern: { type: 'prefix', pattern: '/premium/' },
            access: 'signed',
            signing: { keyGroup: group },
          },
        ],
        errorPages: [{ status: 404, path: '/404.html', responseStatus: 404 }],
        redirects: [{ from: '^/old/(.*)$', to: '/new/$1', status: 301 }],
        headers: { response: { 'X-Content-Type-Options': 'nosniff' } },
        restrictions: { sourceIp: { deny: ['203.0.113.0/24'] } },
        tls: { certificate: `upload:${certificate}` },
      },
    });
  });

  test.afterAll(async ({ api }) => {
    await api.admin('DELETE', `/distributions/${distribution}`);
    await api.admin('DELETE', `/keygroups/${group}`);
    await api.admin('DELETE', `/certificates/${certificate}`);
    await api.deleteBucket(bucket);
  });

  test('access rules tab, tester results, rule editor and YAML view', async ({ page }) => {
    await page.goto(`/buckets/${bucket}/rules`);
    await settle(page);
    await expect(page.getByRole('table', { name: 'Access rules of the bucket' })).toBeVisible();
    expect(await seriousViolations(page), 'rules tab').toEqual([]);

    const tester = page.getByRole('region', { name: 'Pattern tester' });
    await tester.getByRole('textbox', { name: 'Keys' }).fill('img/2026/a.png\ndocs/private/b.txt');
    await tester.getByRole('button', { name: 'Test', exact: true }).click();
    await expect(tester.getByRole('list', { name: 'Results of the test' })).toBeVisible();
    await expect(page.locator('tr[data-highlight]')).not.toHaveCount(0);
    await settle(page);
    expect(await seriousViolations(page), 'tester results').toEqual([]);

    await page.getByRole('button', { name: 'Edit no-private' }).click();
    await expect(page.getByRole('dialog')).toBeVisible();
    await settle(page);
    expect(await seriousViolations(page), 'rule editor').toEqual([]);
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog')).toBeHidden();

    await page.getByText('YAML', { exact: true }).click();
    await expect(page.getByRole('textbox', { name: 'Rules (YAML)' })).toBeVisible();
    expect(await seriousViolations(page), 'YAML view').toEqual([]);
  });

  const pages: [string, () => string][] = [
    ['distributions list', () => '/distributions'],
    ['distribution overview', () => `/distributions/${distribution}`],
    ['distribution behaviors', () => `/distributions/${distribution}/behaviors`],
    ['distribution responses', () => `/distributions/${distribution}/responses`],
    ['distribution security', () => `/distributions/${distribution}/security`],
    ['distribution TLS', () => `/distributions/${distribution}/tls`],
    ['distribution document', () => `/distributions/${distribution}/document`],
    ['key groups', () => '/distributions/-/key-groups'],
    ['certificates', () => '/distributions/-/certificates'],
    ['configuration (block public access)', () => '/configuration'],
  ];
  for (const [name, target] of pages) {
    test(name, async ({ page }) => {
      await page.goto(target());
      await settle(page);
      expect(await seriousViolations(page)).toEqual([]);
    });
  }

  test('wizard steps, behavior editor and URL tester results', async ({ page }) => {
    await page.goto('/distributions/-/new');
    await settle(page);
    const wizard = page.getByRole('region', { name: 'New distribution' });
    expect(await seriousViolations(page), 'wizard origin').toEqual([]);
    await wizard.getByRole('textbox', { name: 'Distribution ID' }).fill(uniqueName('e2e-a11y-w'));
    await wizard.getByRole('textbox', { name: 'Bucket' }).click();
    await page.getByRole('option', { name: bucket, exact: true }).click();
    await wizard.getByRole('button', { name: 'Next' }).click();
    const domains = wizard.getByRole('textbox', { name: 'Domains' });
    await domains.fill(`${distribution}.test`);
    await domains.press('Enter');
    await domains.fill(`new.${uniqueName('e2e-a11y')}.test`);
    await domains.press('Enter');
    await expect(wizard.getByRole('list', { name: 'Availability of the domains' })).toBeVisible();
    await settle(page);
    expect(await seriousViolations(page), 'wizard domains').toEqual([]);

    await page.goto(`/distributions/${distribution}/behaviors`);
    await settle(page);
    const tester = page.getByRole('region', { name: 'URL tester' });
    await tester.getByRole('textbox', { name: 'URL or path' }).fill('/2026/a.png');
    await tester.getByRole('button', { name: 'Test URL' }).click();
    await expect(tester.getByRole('region', { name: 'How the URL is served' })).toBeVisible();
    await expect(page.locator('tr[data-tested]')).toHaveCount(1);
    expect(await seriousViolations(page), 'URL tester results').toEqual([]);
    await page.getByRole('button', { name: 'Add behavior' }).click();
    await expect(page.getByRole('dialog', { name: 'New behavior' })).toBeVisible();
    await settle(page);
    expect(await seriousViolations(page), 'behavior editor').toEqual([]);
  });

  test('dark colour scheme', async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Toggle colour scheme' }).click();
    await expect(page.locator('html')).toHaveAttribute('data-mantine-color-scheme', 'dark');
    for (const target of [
      `/buckets/${bucket}/rules`,
      '/distributions',
      `/distributions/${distribution}/behaviors`,
      '/distributions/-/certificates',
    ]) {
      await page.goto(target);
      await settle(page);
      expect(await seriousViolations(page), target).toEqual([]);
    }
  });

  test('upload dialog of the certificates and creation of a key group', async ({ page }) => {
    await page.goto('/distributions/-/certificates');
    await settle(page);
    await page.getByRole('button', { name: 'Upload certificate' }).click();
    await expect(page.getByRole('dialog', { name: 'Upload a certificate' })).toBeVisible();
    expect(await seriousViolations(page), 'certificate upload').toEqual([]);
    await page.keyboard.press('Escape');

    await page.goto('/distributions/-/key-groups');
    await settle(page);
    await page.getByRole('button', { name: 'Create key group' }).click();
    await expect(page.getByRole('dialog', { name: 'New key group' })).toBeVisible();
    expect(await seriousViolations(page), 'key group dialog').toEqual([]);
  });
});
