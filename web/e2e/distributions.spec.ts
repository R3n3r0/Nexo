import { readFile } from 'node:fs/promises';

import type { Browser, BrowserContext } from '@playwright/test';

import { type NexoClient, uniqueName } from './support/api';
import { selfSigned } from './support/certs';
import { contextAs, expect, test } from './support/fixtures';
import { type HostProxy, startHostProxy } from './support/hostProxy';

/** A browser context whose requests to the *.test domains reach the server under test. */
function visitor(browser: Browser, proxy: HostProxy): Promise<BrowserContext> {
  return browser.newContext({ proxy: { server: proxy.server } });
}

/** Deletes the distributions created by a test, ignoring those already gone. */
async function removeDistributions(api: NexoClient, ids: string[]) {
  for (const id of ids) {
    await api.admin('DELETE', `/distributions/${id}`);
  }
}

/** D-DST-01..07 against the server (docs/10 §10.6 TD-D-01..05). */
test.describe('distributions', () => {
  let proxy: HostProxy;
  let bucket: string;
  const created: string[] = [];

  test.beforeAll(async ({ nexo }) => {
    proxy = await startHostProxy(nexo.s3URL);
  });

  test.afterAll(async () => {
    await proxy.close();
  });

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-dist');
    await api.createBucket(bucket);
    created.length = 0;
  });

  test.afterEach(async ({ api }) => {
    await removeDistributions(api, created);
    await api.deleteBucket(bucket);
  });

  test('TD-D-01 the wizard creates a distribution with two domains, its DNS records and routing', async ({
    page,
    api,
    browser,
  }) => {
    const id = uniqueName('e2e-wiz');
    created.push(id);
    const apex = `${id}.test`;
    const www = `www.${id}.test`;
    await api.putObject(bucket, 'index.html', `<h1>Site of ${id}</h1>`, 'text/html');

    await page.goto('/distributions/-/new');
    const wizard = page.getByRole('region', { name: 'New distribution' });
    await wizard.getByRole('textbox', { name: 'Distribution ID' }).fill(id);
    await wizard.getByRole('textbox', { name: 'Bucket' }).click();
    await page.getByRole('option', { name: bucket, exact: true }).click();
    await wizard.getByRole('button', { name: 'Next' }).click();

    // Two domains, checked against the routing table of the cluster.
    const domains = wizard.getByRole('textbox', { name: 'Domains' });
    for (const domain of [www, apex]) {
      await domains.fill(domain);
      await domains.press('Enter');
    }
    const states = wizard.getByRole('list', { name: 'Availability of the domains' });
    await expect(states.getByText('Available', { exact: true })).toHaveCount(2);

    // The DNS records: a CNAME for the subdomain, A/AAAA for the apex of the zone.
    await wizard.getByRole('textbox', { name: 'Target of the records' }).fill('edge.nexo.test');
    const records = wizard.getByRole('table', { name: 'DNS records to create' });
    const recordsOf = (name: string) =>
      records.getByRole('row').filter({ has: page.getByRole('cell', { name, exact: true }) });
    await expect(recordsOf(www)).toHaveCount(1);
    await expect(recordsOf(www)).toContainText('CNAME');
    await expect(recordsOf(www)).toContainText('edge.nexo.test.');
    await expect(recordsOf(apex)).toHaveCount(2);
    await expect(recordsOf(apex).filter({ hasText: 'AAAA' })).toHaveCount(1);

    // Mode, access and TLS: website, public, TLS terminated by a proxy.
    for (let step = 0; step < 4; step += 1) {
      await wizard.getByRole('button', { name: 'Next' }).click();
    }
    await expect(wizard.getByLabel('Document of the distribution')).toContainText(
      `aliases: [${www}, ${apex}]`,
    );
    await wizard.getByRole('button', { name: 'Create distribution' }).click();
    await expect(wizard.getByText(`Distribution ${id} created`)).toBeVisible();
    const stored = await api.call<{ aliases: string[]; enabled: boolean; mode: string }>(
      'GET',
      `/distributions/${id}`,
    );
    expect(stored).toMatchObject({ aliases: [www, apex], enabled: true, mode: 'website' });

    // Both domains are routed to the distribution...
    await wizard.getByRole('button', { name: 'Verify the routing' }).click();
    const routing = wizard.getByRole('list', { name: 'Routing of the domains' });
    await expect(routing.getByText('Routed', { exact: true })).toHaveCount(2);
    // ...and serve the bucket to a browser whose DNS points them to the server.
    const context = await visitor(browser, proxy);
    try {
      const site = await context.newPage();
      for (const domain of [www, apex]) {
        const res = await site.goto(`http://${domain}/`);
        expect(res?.status(), domain).toBe(200);
        await expect(site.getByRole('heading', { name: `Site of ${id}` })).toBeVisible();
      }
    } finally {
      await context.close();
    }
    expect(proxy.hosts).toEqual(expect.arrayContaining([www, apex]));

    // The list shows the new distribution with its domains and bucket.
    await wizard.getByRole('button', { name: 'Open the distribution' }).click();
    await expect(page).toHaveURL(new RegExp(`/distributions/${id}$`));
    await page.goto('/distributions');
    const row = page
      .getByRole('table', { name: 'Distributions' })
      .getByRole('row')
      .filter({ has: page.getByRole('link', { name: id, exact: true }) });
    await expect(row).toContainText(www);
    await expect(row).toContainText(bucket);
  });

  test('TD-D-02 a behavior with SPA fallback and an error page, tested then served on the domain', async ({
    page,
    api,
    browser,
  }) => {
    const id = uniqueName('e2e-spa');
    created.push(id);
    const host = `${id}.test`;
    await api.putObject(bucket, 'index.html', '<h1>SPA home</h1>', 'text/html');
    await api.putObject(bucket, '404.html', '<h1>Page not found</h1>', 'text/html');
    await api.putObject(bucket, 'assets/app.js', 'console.log(1);', 'text/javascript');
    await api.call('POST', '/distributions', {
      json: {
        id,
        enabled: true,
        aliases: [host],
        origin: { bucket },
        mode: 'website',
        defaultBehavior: { access: 'public', indexDocument: 'index.html' },
      },
    });

    // A behavior for the routes of the application, with its SPA fallback.
    await page.goto(`/distributions/${id}/behaviors`);
    await page.getByRole('button', { name: 'Add behavior' }).click();
    const dialog = page.getByRole('dialog', { name: 'New behavior' });
    await dialog.getByRole('textbox', { name: 'Path pattern' }).fill('/app/**');
    await dialog.getByRole('textbox', { name: 'SPA fallback' }).fill('/index.html');
    await dialog.getByRole('button', { name: 'Apply' }).click();
    await expect(dialog).toBeHidden();
    const behaviors = page.getByRole('table', { name: 'Behaviors of the distribution' });
    await expect(behaviors.getByText('/app/**')).toBeVisible();

    // The error page of the 404 of every other path.
    await page.getByRole('tab', { name: 'Responses' }).click();
    const errorPages = page.getByRole('region', { name: 'Error pages' });
    await errorPages.getByRole('button', { name: 'Add error page' }).click();
    await expect(errorPages.getByRole('textbox', { name: 'Page' })).toHaveValue('/404.html');
    const bar = page.getByRole('region', { name: 'Unsaved changes' });
    await bar.getByRole('button', { name: 'Save changes' }).click();
    await expect(bar).toBeHidden();
    const stored = await api.call<{
      behaviors: { pathPattern: { type: string; pattern: string }; spaFallback?: string }[];
      errorPages: { status: number; path: string; responseStatus?: number }[];
    }>('GET', `/distributions/${id}`);
    expect(stored.behaviors).toEqual([
      expect.objectContaining({
        pathPattern: { type: 'glob', pattern: '/app/**' },
        spaFallback: '/index.html',
      }),
    ]);
    expect(stored.errorPages).toEqual([{ status: 404, path: '/404.html', responseStatus: 404 }]);

    // The URL tester names the behavior serving a URL.
    await page.getByRole('tab', { name: 'Behaviors' }).click();
    const tester = page.getByRole('region', { name: 'URL tester' });
    await tester.getByRole('textbox', { name: 'URL or path' }).fill(`http://${host}/app/settings`);
    await tester.getByRole('button', { name: 'Test URL' }).click();
    const result = tester.getByRole('region', { name: 'How the URL is served' });
    await expect(result.getByText('Behavior 1 (/app/**)')).toBeVisible();
    await expect(result.getByText(`${bucket}/app/settings`)).toBeVisible();
    await tester.getByRole('textbox', { name: 'URL or path' }).fill(`http://${host}/assets/app.js`);
    await tester.getByRole('button', { name: 'Test URL' }).click();
    await expect(result.getByText('Default behavior', { exact: true })).toBeVisible();

    // From a browser on the domain: the routes of the application get the
    // index (200), a missing page the error page with its status.
    const context = await visitor(browser, proxy);
    try {
      const site = await context.newPage();
      const route = await site.goto(`http://${host}/app/settings/profile`);
      expect(route?.status()).toBe(200);
      await expect(site.getByRole('heading', { name: 'SPA home' })).toBeVisible();
      const missing = await site.goto(`http://${host}/missing/page.html`);
      expect(missing?.status()).toBe(404);
      await expect(site.getByRole('heading', { name: 'Page not found' })).toBeVisible();
      const home = await site.goto(`http://${host}/`);
      expect(home?.status()).toBe(200);
      await expect(site.getByRole('heading', { name: 'SPA home' })).toBeVisible();
      const asset = await site.request.get(`http://${host}/assets/app.js`);
      expect(asset.status()).toBe(200);
      expect(await asset.text()).toBe('console.log(1);');
    } finally {
      await context.close();
    }
  });

  test('TD-D-03 the private key of a key group is shown once and signs a URL that opens the file', async ({
    page,
    api,
    browser,
  }) => {
    const group = uniqueName('e2e-kg');
    const id = uniqueName('e2e-dl');
    created.push(id);
    const host = `${id}.test`;
    await api.putObject(bucket, 'files/report.txt', 'premium report', 'text/plain');
    try {
      await page.goto('/distributions/-/key-groups');
      await page.getByRole('button', { name: 'Create key group' }).click();
      const dialog = page.getByRole('dialog', { name: 'New key group' });
      await dialog.getByRole('textbox', { name: 'Key group ID' }).fill(group);
      await dialog.getByRole('button', { name: 'Create' }).click();

      // The private key: shown, downloadable, then gone.
      const reveal = page.getByRole('dialog', { name: 'Private key' });
      const shown =
        (await reveal.getByLabel('Private key (PKCS 8)', { exact: true }).textContent()) ?? '';
      expect(shown).toContain('-----BEGIN PRIVATE KEY-----');
      const downloading = page.waitForEvent('download');
      await reveal.getByRole('button', { name: new RegExp(`^Download ${group}-`) }).click();
      const download = await downloading;
      const file = await download.path();
      expect((await readFile(file, 'utf8')).trim()).toBe(shown.trim());
      const done = reveal.getByRole('button', { name: 'Done' });
      await expect(done).toBeDisabled();
      await reveal
        .getByRole('checkbox', { name: 'I have stored the secret in a safe place' })
        .check();
      await done.click();
      await expect(reveal).toBeHidden();
      const card = page.getByRole('region', { name: `Key group ${group}` });
      await expect(card.getByText('1 key', { exact: true })).toBeVisible();
      await page.reload();
      await expect(card).toBeVisible();
      await expect(page.getByText('BEGIN PRIVATE KEY')).toHaveCount(0);
      expect(JSON.stringify(await api.call('GET', `/keygroups/${group}`))).not.toContain(
        'PRIVATE KEY',
      );

      // A distribution whose content needs a signature of the group.
      await api.call('POST', '/distributions', {
        json: {
          id,
          enabled: true,
          aliases: [host],
          origin: { bucket },
          mode: 'website',
          defaultBehavior: { access: 'signed', signing: { keyGroup: group } },
        },
      });
      await page.goto(`/distributions/${id}/security`);
      const signer = page.getByRole('region', { name: 'Signed URLs and cookies' });
      await signer
        .getByRole('textbox', { name: 'URL or path' })
        .fill(`http://${host}/files/report.txt`);
      await signer.getByRole('textbox', { name: 'Private key (PEM)' }).fill(shown);
      await signer.getByRole('button', { name: 'Sign URL' }).click();
      const signed = (await signer.getByLabel('Signed URL', { exact: true }).textContent()) ?? '';
      expect(signed).toMatch(
        new RegExp(`^http://${host}/files/report\\.txt\\?Expires=\\d+&Signature=.+&Key-Pair-Id=`),
      );

      const context = await visitor(browser, proxy);
      try {
        const site = await context.newPage();
        const ok = await site.goto(signed);
        expect(ok?.status()).toBe(200);
        await expect(site.locator('body')).toContainText('premium report');
        const unsigned = await site.request.get(`http://${host}/files/report.txt`);
        expect(unsigned.status()).toBe(403);
        const altered = await site.request.get(
          signed.replace(
            /Signature=([^&])/,
            (_m, c: string) => `Signature=${c === 'A' ? 'B' : 'A'}`,
          ),
        );
        expect(altered.status()).toBe(403);
      } finally {
        await context.close();
      }
    } finally {
      await removeDistributions(api, created.splice(0));
      await api.admin('DELETE', `/keygroups/${group}`);
    }
  });

  test('TD-D-04 an expiring certificate is highlighted; an invalid one is refused', async ({
    page,
    api,
    nexo,
  }) => {
    const domain = `${uniqueName('e2e-cert')}.test`;
    const lasting = `${uniqueName('e2e-cert')}.test`;
    const id = uniqueName('e2e-tls');
    const soon = await selfSigned(nexo.workDir, domain, 10);
    const later = await selfSigned(nexo.workDir, lasting, 200);
    try {
      await page.goto('/distributions/-/certificates');
      await page.getByRole('button', { name: 'Upload certificate' }).click();
      const dialog = page.getByRole('dialog', { name: 'Upload a certificate' });
      // Not a certificate: refused with the reason of the server.
      await dialog.getByRole('textbox', { name: 'Certificate (PEM)' }).fill('not a certificate');
      await dialog.getByRole('textbox', { name: 'Private key (PEM)' }).fill('nope');
      await dialog.getByRole('button', { name: 'Upload certificate' }).click();
      await expect(dialog.getByText('The certificate was refused')).toBeVisible();
      await expect(dialog.getByText(/failed to find any PEM data/)).toBeVisible();
      // A key of another certificate: refused too.
      await dialog.getByRole('textbox', { name: 'Certificate (PEM)' }).fill(soon.certificatePem);
      await dialog.getByRole('textbox', { name: 'Private key (PEM)' }).fill(later.privateKeyPem);
      await dialog.getByRole('button', { name: 'Upload certificate' }).click();
      await expect(dialog.getByText(/private key does not match/)).toBeVisible();

      // The valid one, 10 days left: uploaded and highlighted.
      await dialog.getByRole('textbox', { name: 'Private key (PEM)' }).fill(soon.privateKeyPem);
      await dialog.getByRole('button', { name: 'Upload certificate' }).click();
      await expect(dialog).toBeHidden();
      await api.call('POST', '/certificates', { json: later });
      const table = page.getByRole('table', { name: 'TLS certificates' });
      const rowOf = (name: string) =>
        table.getByRole('row').filter({ has: page.getByText(name, { exact: true }) });
      await expect(rowOf(domain).locator('[data-expiry="expiring"]')).toHaveText(
        'Expires in 10 days',
      );
      await page.reload();
      await expect(rowOf(lasting).locator('[data-expiry="ok"]')).toHaveText(
        /^Valid for 20\d days$/,
      );
      await expect(page.getByText(/expires? within 21 days or ha(s|ve) expired\./)).toBeVisible();

      // The distribution using it shows the expiry in the list too.
      await api.call('POST', '/distributions', {
        json: {
          id,
          enabled: true,
          aliases: [domain],
          origin: { bucket },
          mode: 'website',
          tls: { certificate: `upload:${domain}` },
        },
      });
      created.push(id);
      await page.goto('/distributions');
      const row = page
        .getByRole('table', { name: 'Distributions' })
        .getByRole('row')
        .filter({ has: page.getByRole('link', { name: id, exact: true }) });
      await expect(row.locator('[data-expiry="expiring"]')).toHaveText('Expires in 10 days');
    } finally {
      await removeDistributions(api, created.splice(0));
      await api.admin('DELETE', `/certificates/${domain}`);
      await api.admin('DELETE', `/certificates/${lasting}`);
    }
  });

  test('TD-D-05 a user without admin:ManageDistributions neither sees nor changes the distributions', async ({
    browser,
    nexo,
    api,
  }) => {
    const id = uniqueName('e2e-hidden');
    await api.call('POST', '/distributions', {
      json: { id, enabled: true, aliases: [`${id}.test`], origin: { bucket }, mode: 'website' },
    });
    created.push(id);
    const context = await contextAs(browser, nexo, nexo.users.member);
    try {
      const page = await context.newPage();
      await page.goto('/');
      const nav = page.getByRole('navigation').first();
      await expect(nav.getByRole('link', { name: 'Buckets' })).toBeVisible();
      await expect(nav.getByRole('link', { name: 'Distributions' })).toHaveCount(0);

      await page.goto('/distributions');
      await expect(page.getByText('You may not manage the distributions.')).toBeVisible();
      await expect(page.getByRole('link', { name: id })).toHaveCount(0);
      await page.goto('/distributions/-/new');
      await expect(
        page.getByText('You do not have the permission admin:ManageDistributions.'),
      ).toBeVisible();
      await expect(page.getByRole('region', { name: 'New distribution' })).toHaveCount(0);

      // The server refuses the member whatever the console shows.
      const member = api.as(nexo.users.member);
      expect((await member.admin('GET', '/distributions')).status).toBe(403);
      expect(
        (
          await member.admin('POST', '/distributions', {
            json: { id: `${id}-x`, aliases: [`x-${id}.test`], origin: { bucket } },
          })
        ).status,
      ).toBe(403);
      expect((await member.admin('DELETE', `/distributions/${id}`)).status).toBe(403);
      expect((await api.call<{ id: string }>('GET', `/distributions/${id}`)).id).toBe(id);
    } finally {
      await context.close();
    }
  });
});
