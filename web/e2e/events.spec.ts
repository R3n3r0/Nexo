import { uniqueName } from './support/api';
import { Nexoctl } from './support/cli';
import { expect, test } from './support/fixtures';
import { eventNameOf, startWebhook, type Webhook } from './support/webhook';

/** A subscription of the Admin API (GET /buckets/{bucket}/events). */
interface Subscription {
  id?: string;
  arn: string;
  events: string[];
  prefix?: string;
  suffix?: string;
}

// covers: D-BKT-04
test.describe('bucket events (D-BKT-04)', () => {
  let bucket: string;
  let target: string;
  let webhook: Webhook;

  test.beforeEach(async ({ api }) => {
    bucket = uniqueName('e2e-events');
    target = uniqueName('hook').replace(/[^a-z0-9-]/g, '');
    webhook = await startWebhook();
    await api.createBucket(bucket);
  });

  test.afterEach(async ({ api }) => {
    await api.deleteBucket(bucket);
    await api.admin('DELETE', `/config/notify_webhook:${target}`);
    await webhook.close();
  });

  // covers: D-BKT-03, D-BKT-04
  test('TD-E-14 a webhook target receives the test event and the subscribed events, visible from the CLI', async ({
    page,
    api,
    nexo,
  }) => {
    // The target, with the form of its type on the configuration page.
    await page.goto('/configuration');
    const targets = page.getByRole('region', { name: 'Notification targets' });
    await targets.getByRole('button', { name: 'Add target' }).click();
    const form = page.getByRole('dialog', { name: 'New notification target' });
    await expect(form.getByRole('textbox', { name: 'Type' })).toHaveValue('Webhook');
    await form.getByRole('textbox', { name: 'Target ID' }).fill(target);
    await expect(form.getByText(`Configuration subsystem: notify_webhook:${target}`)).toBeVisible();
    await form.getByRole('textbox', { name: 'endpoint' }).fill(webhook.url);
    await form.getByRole('button', { name: 'Save target' }).click();
    await expect(page.getByText(`Target notify_webhook:${target} saved.`)).toBeVisible();
    const ref = `${target}:webhook`;
    await expect(targets.getByText(ref, { exact: true })).toBeVisible();

    // The subscription, from the events tab of the bucket.
    await page.goto(`/buckets/${bucket}/events`);
    const section = page.getByRole('region', { name: 'Event subscriptions' });
    await expect(section.getByText('No subscription.')).toBeVisible();
    await section.getByRole('button', { name: 'Add subscription' }).click();
    const editor = page.getByRole('dialog', { name: 'New event subscription' });
    await editor.getByRole('textbox', { name: 'Target' }).click();
    await page.getByRole('option', { name: new RegExp(`^${ref}`) }).click();
    await editor.getByRole('textbox', { name: 'Key prefix' }).fill('images/');
    await editor.getByRole('button', { name: 'Save subscription' }).click();
    await expect(page.getByText('Event subscriptions saved.')).toBeVisible();
    const table = section.getByRole('table', { name: 'Event subscriptions' });
    await expect(table.getByText(ref)).toBeVisible();
    await expect(table.getByText('s3:ObjectCreated:*')).toBeVisible();

    // The test event, delivered at once to the webhook of the test.
    const state = page.getByRole('region', { name: 'Notification targets' });
    await state.getByRole('button', { name: `Send a test event to ${ref}` }).click();
    await expect(state.getByText(`Test event delivered to ${ref}.`)).toBeVisible();
    await expect.poll(() => webhook.deliveries.map(eventNameOf)).toContain('s3:TestEvent');

    // A write under the prefix is notified, one outside is not.
    await api.putObject(bucket, 'images/cat.jpg', 'meow', 'image/jpeg');
    await api.putObject(bucket, 'docs/readme.txt', 'skip', 'text/plain');
    await expect
      .poll(
        () =>
          webhook.deliveries
            .filter((d) => eventNameOf(d) === 's3:ObjectCreated:Put')
            .map(
              (d) =>
                (d.body as { Records: { s3: { object: { key: string } } }[] }).Records[0]?.s3.object
                  .key,
            ),
        { timeout: 20_000 },
      )
      .toEqual(['images/cat.jpg']);

    // The Admin API and the CLI see the same subscription.
    const stored = await api.call<{ subscriptions: Subscription[] }>(
      'GET',
      `/buckets/${bucket}/events`,
    );
    expect(stored.subscriptions).toEqual([
      expect.objectContaining({
        arn: `arn:nexo:sqs:${nexo.region}:${target}:webhook`,
        events: ['s3:ObjectCreated:*'],
        prefix: 'images/',
      }),
    ]);
    const listed = await new Nexoctl(nexo).ok('event', 'ls', `e2e/${bucket}`);
    expect(listed).toContain(`arn:nexo:sqs:${nexo.region}:${target}:webhook`);
    expect(listed).toContain('s3:ObjectCreated:*');
  });

  test('asks for at least one event before saving', async ({ page, api }) => {
    await page.goto(`/buckets/${bucket}/events`);
    const section = page.getByRole('region', { name: 'Event subscriptions' });
    await section.getByRole('button', { name: 'Add subscription' }).click();
    const editor = page.getByRole('dialog', { name: 'New event subscription' });
    const events = editor.getByRole('textbox', { name: 'Events' });
    await events.click();
    await events.press('Backspace');
    await events.press('Escape');
    await editor.getByRole('button', { name: 'Save subscription' }).click();
    await expect(editor.getByText('Choose at least one event.')).toBeVisible();
    expect(
      (await api.call<{ subscriptions: Subscription[] }>('GET', `/buckets/${bucket}/events`))
        .subscriptions,
    ).toEqual([]);
  });
});
