import type { Locator, Page } from '@playwright/test';

import { NexoClient, uniqueName } from '../support/api';
import { expect, test } from '../support/fixtures';
import type { NexoProcess } from '../support/nexo';

/** The endpoint ("host:port") a node reports for itself. */
const hostOf = (n: NexoProcess) => new URL(n.s3URL).host;

/** The row of a node in a table, matched on its exact endpoint. */
function rowOf(table: Locator, node: NexoProcess): Locator {
  return table
    .getByRole('row')
    .filter({ has: table.page().getByText(hostOf(node), { exact: true }) });
}

/**
 * The background healing counters of a node on the healing page, once the
 * page shows them (the server information waits for the peers that do not
 * answer); undefined while the row is missing.
 */
async function queueOf(
  page: Page,
  node: NexoProcess,
): Promise<{ pending: number; healed: number } | undefined> {
  const queues = page.getByRole('table', { name: 'Background healing queue of the nodes' });
  await expect(queues).toBeVisible({ timeout: 30_000 });
  const row = rowOf(queues, node);
  if ((await row.count()) === 0) {
    return undefined;
  }
  const cells = (await row.first().getByRole('cell').allTextContents()).map((t) =>
    Number(t.replace(/\D/g, '')),
  );
  return { pending: cells[1] ?? 0, healed: cells[2] ?? 0 };
}

test.describe('cluster pages (suite cluster)', () => {
  // covers: D-CLU-01, D-CLU-02
  test('TD-E-15 a stopped node and its drives are highlighted, the healing shows when it is back', async ({
    page,
    api,
    nexo,
  }) => {
    const nodes = nexo.nodes ?? [];
    expect(nodes).toHaveLength(4);
    const [first, stopped] = nodes;
    if (first === undefined || stopped === undefined) throw new Error('cluster state');
    const bucket = uniqueName('e2e-heal');
    const keys = ['a.txt', 'b.txt', 'c.txt'];

    // Every node, drive and set online.
    await page.goto('/cluster');
    await expect(page.getByRole('heading', { name: 'Cluster', level: 2 })).toBeVisible();
    const nodesTable = page.getByRole('table', { name: 'Nodes' });
    await expect(nodesTable.locator('tbody tr[data-state="online"]')).toHaveCount(4);
    await expect(rowOf(nodesTable, first)).toContainText('This node');
    await expect(page.getByRole('region', { name: 'Deployment mode' })).toContainText(
      'Distributed cluster',
    );
    await expect(page.getByRole('region', { name: 'Health' })).toContainText('Healthy');
    const sets = page.getByRole('table', { name: 'Erasure sets of pool 1' });
    await expect(sets).toContainText('8 of 8');
    await expect(page.getByRole('region', { name: 'Pool 1' })).toContainText(
      'read quorum 4, write quorum 5',
    );

    process.kill(stopped.pid, 'SIGSTOP');
    try {
      // The frozen node and its drives are reported offline and
      // highlighted; the set stays writable (6 of 8 drives).
      const row = rowOf(nodesTable, stopped);
      await expect(row).toHaveAttribute('data-state', 'offline', { timeout: 60_000 });
      await expect(row).toContainText('Offline');
      await expect(page.getByRole('alert').filter({ hasText: '1 node offline' })).toContainText(
        hostOf(stopped),
      );
      const drives = page.getByRole('table', { name: 'Drives of pool 1' });
      // The row of the node and the rows of its two drives.
      await expect(drives.locator('tr[data-state="offline"]')).toHaveCount(3);
      await expect(sets).toContainText('Degraded');
      await expect(sets).toContainText('6 of 8');

      await page.goto('/');
      const health = page.getByRole('region', { name: 'Cluster health' });
      await expect(health).toContainText('Degraded', { timeout: 30_000 });
      await expect(health).toContainText('1 node offline');
      await expect(health).toContainText(hostOf(stopped));

      // Writes go on with the quorum; the copies of the stopped node wait
      // in the background healing of the node that wrote them.
      await api.createBucket(bucket);
      for (const key of keys) {
        await api.putObject(bucket, key, `content of ${key}`, 'text/plain');
      }
      await page.goto('/healing');
      await expect
        .poll(async () => (await queueOf(page, first))?.pending ?? 0, { timeout: 30_000 })
        .toBeGreaterThanOrEqual(keys.length);

      // A dry run started from the page reports the drives of the stopped
      // node offline, drive by drive.
      await page.getByRole('textbox', { name: 'Bucket' }).click();
      await page.getByRole('option', { name: bucket }).click();
      await page.getByRole('checkbox', { name: 'Dry run: only report' }).check();
      await page.getByRole('button', { name: 'Start healing' }).click();
      const job = page.getByRole('region', { name: /^Job / });
      await expect(job.getByText('Done', { exact: true })).toBeVisible({ timeout: 60_000 });
      const results = job.getByRole('table', { name: 'Results by drive' });
      await expect(results.locator('tbody tr')).toHaveCount(8);
      await expect(results.locator('tr[data-state="offline"]')).toHaveCount(2);
    } finally {
      process.kill(stopped.pid, 'SIGCONT');
    }

    // Back online: the background healing rebuilds the copies the node
    // missed, and the healing page shows them repaired.
    await page.goto('/healing');
    await expect
      .poll(
        async () => {
          const q = await queueOf(page, first);
          return q !== undefined && q.pending === 0 && q.healed >= keys.length;
        },
        { timeout: 90_000 },
      )
      .toBe(true);

    await page.goto('/cluster');
    await expect(rowOf(nodesTable, stopped)).toHaveAttribute('data-state', 'online', {
      timeout: 60_000,
    });
    await expect(page.getByRole('region', { name: 'Health' })).toContainText('Healthy', {
      timeout: 30_000,
    });
    await expect(page.getByRole('alert')).toHaveCount(0);

    // A heal of the bucket finds nothing left to repair, and the objects
    // read back from the node that was stopped.
    await page.goto('/healing');
    await page.getByRole('textbox', { name: 'Bucket' }).click();
    await page.getByRole('option', { name: bucket }).click();
    await page.getByRole('button', { name: 'Start healing' }).click();
    const job = page.getByRole('region', { name: /^Job / });
    await expect(job.getByText('Done', { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(
      job.getByRole('table', { name: 'Results by drive' }).locator('tr[data-state="online"]'),
    ).toHaveCount(8);
    const viaStopped = new NexoClient(stopped);
    for (const key of keys) {
      const res = await viaStopped.s3('GET', `/${bucket}/${key}`);
      expect(res.status, key).toBe(200);
      expect(await res.text()).toBe(`content of ${key}`);
    }
    await api.deleteBucket(bucket);
  });
});
