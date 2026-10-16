import { uniqueName } from './support/api';
import { expect, test } from './support/fixtures';

interface DnsProvider {
  name: string;
  hasCredentials: boolean;
  usedBy: string[];
}

/**
 * D-DST-07: the DNS providers of the ACME DNS-01 challenge against the
 * server. The provider is an RFC 2136 one whose nameserver does not
 * answer (127.0.0.1:1): its test fails at once at the "present" step, and
 * the distribution using it stays disabled, so that no ACME order leaves
 * the machine.
 */
test.describe('DNS providers', () => {
  // covers: D-DST-07
  test('TD-D-06 a DNS provider is managed, tested and bound to a wildcard domain', async ({
    page,
    api,
  }) => {
    const name = uniqueName('e2e-dns');
    const id = uniqueName('e2e-wild');
    const bucket = uniqueName('e2e-dns');
    const secret = 'ZTJlLXRzaWctc2VjcmV0LXZhbHVl';
    const domain = `*.${uniqueName('e2e')}.test`;
    await api.createBucket(bucket);
    try {
      await page.goto('/distributions/-/certificates');
      const section = page.getByRole('region', { name: 'DNS providers' });
      await section.getByRole('button', { name: 'Add DNS provider' }).click();
      const dialog = page.getByRole('dialog', { name: 'Add a DNS provider' });
      await dialog.getByRole('textbox', { name: 'Name' }).fill(name);
      await dialog.getByRole('textbox', { name: 'Type' }).click();
      await page.getByRole('option', { name: 'RFC 2136 (TSIG)' }).click();
      await dialog.getByRole('textbox', { name: 'Nameserver' }).fill('127.0.0.1:1');
      await dialog.getByRole('textbox', { name: 'TSIG key name' }).fill('nexo-acme');
      // A secret that is not base64: refused by the server.
      await dialog.getByLabel(/TSIG secret/).fill('not base64!');
      await dialog.getByRole('button', { name: 'Save' }).click();
      await expect(dialog.getByText('The DNS provider was refused')).toBeVisible();
      await dialog.getByLabel(/TSIG secret/).fill(secret);
      await dialog.getByRole('button', { name: 'Save' }).click();
      await expect(dialog).toBeHidden();

      const table = section.getByRole('table', { name: 'DNS providers' });
      const row = table.getByRole('row').filter({ has: page.getByText(name, { exact: true }) });
      await expect(row.getByText('127.0.0.1:1 (nexo-acme)')).toBeVisible();
      await expect(row.getByText('Stored')).toBeVisible();
      // The secret is sealed: never returned by the API nor shown.
      const listed = await api.call<{ items: DnsProvider[] }>('GET', '/dns-providers');
      expect(JSON.stringify(listed)).not.toContain(secret);
      await expect(page.locator('body')).not.toContainText(secret);

      // The test publishes nothing: the nameserver does not answer.
      await row.getByRole('button', { name: `Test the DNS provider ${name}` }).click();
      const test = page.getByRole('dialog', { name: `Test the DNS provider ${name}` });
      await test.getByRole('textbox', { name: 'Domain' }).fill('example.test');
      await test.getByRole('button', { name: 'Run the test' }).click();
      await expect(test.getByText(/Failed at the present step/)).toBeVisible();
      await page.keyboard.press('Escape');

      // A wildcard domain with an automatic certificate needs DNS-01.
      await api.call('POST', '/distributions', {
        json: { id, enabled: false, aliases: [domain], origin: { bucket }, mode: 'website' },
      });
      await page.goto(`/distributions/${id}/tls`);
      const panel = page.getByRole('region', { name: 'TLS' });
      // The segmented control: its radio inputs are hidden behind their labels.
      await panel
        .getByRole('radiogroup', { name: 'Certificate' })
        .getByText('ACME', { exact: true })
        .click();
      await expect(panel.getByRole('radio', { name: 'ACME' })).toBeChecked();
      await expect(
        panel.getByText(`The wildcard domain ${domain} needs the DNS-01 challenge`, {
          exact: false,
        }),
      ).toBeVisible();
      await panel.getByRole('textbox', { name: 'ACME challenge' }).click();
      await page.getByRole('option', { name: /DNS-01/ }).click();
      await panel.getByRole('textbox', { name: 'DNS provider' }).click();
      await page.getByRole('option', { name: new RegExp(`^${name} `) }).click();
      await page.getByRole('button', { name: 'Save changes' }).click();
      await expect(page.getByRole('region', { name: 'Unsaved changes' })).toBeHidden();
      const saved = await api.call<{ tls: unknown }>('GET', `/distributions/${id}`);
      expect(saved.tls).toEqual({ certificate: 'auto', challenge: 'dns-01', dnsProvider: name });

      // In use: the provider cannot be deleted.
      await page.goto('/distributions/-/certificates');
      await row.getByRole('button', { name: `Delete the DNS provider ${name}` }).click();
      const confirm = page.getByRole('dialog', { name: 'Delete the DNS provider' });
      await confirm.getByRole('button', { name: 'Delete' }).click();
      await expect(confirm.getByText(new RegExp(`used by the distributions ${id}`))).toBeVisible();
    } finally {
      await api.admin('DELETE', `/distributions/${id}`);
      await api.admin('DELETE', `/dns-providers/${name}`);
      await api.deleteBucket(bucket);
    }
  });
});
