import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { BucketEventsTab } from './BucketEventsTab';

const routes = [{ path: '/buckets/:bucket/events', element: <BucketEventsTab /> }];

function open(services = createMockServices({ initialAccessKey: 'nexoadmin' })) {
  const result = renderRoutes(routes, { initialEntries: ['/buckets/media/events'], services });
  return { ...result, user: userEvent.setup() };
}

async function enabledButton(name: string) {
  await waitFor(() => {
    expect(screen.getByRole('button', { name })).toBeEnabled();
  });
  return screen.getByRole('button', { name });
}

/** D-BKT-04: event subscriptions and the state of the targets. */
describe('BucketEventsTab', () => {
  it('subscribes a target to events with filters', async () => {
    const { user, services } = open();
    expect(await screen.findByText('No subscription.')).toBeInTheDocument();
    await user.click(await enabledButton('Add subscription'));
    const dialog = await screen.findByRole('dialog', { name: 'New event subscription' });
    // The first target is chosen by default; take the kafka one.
    await user.click(within(dialog).getByRole('textbox', { name: 'Target' }));
    await user.click(await screen.findByRole('option', { name: /^orders:kafka/ }));
    await user.click(within(dialog).getByRole('textbox', { name: 'Events' }));
    await user.click(await screen.findByRole('option', { name: 's3:ObjectRemoved:Delete' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Key prefix' }), 'images/');
    await user.type(within(dialog).getByRole('textbox', { name: 'Key suffix' }), '.jpg');
    await user.click(within(dialog).getByRole('button', { name: 'Save subscription' }));

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const table = screen.getByRole('table', { name: 'Event subscriptions' });
    expect(within(table).getByText('orders:kafka')).toBeInTheDocument();
    expect(within(table).getByText('s3:ObjectCreated:*')).toBeInTheDocument();
    expect(within(table).getByText('s3:ObjectRemoved:Delete')).toBeInTheDocument();
    expect(within(table).getByText('Prefix images/ · Suffix .jpg')).toBeInTheDocument();
    const stored = await services.notifications.get('media');
    expect(stored.subscriptions).toEqual([
      expect.objectContaining({
        arn: 'arn:nexo:sqs:us-east-1:orders:kafka',
        events: ['s3:ObjectCreated:*', 's3:ObjectRemoved:Delete'],
        prefix: 'images/',
        suffix: '.jpg',
      }),
    ]);
  });

  it('validates the subscription before sending', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const set = vi.spyOn(services.notifications, 'set');
    const { user } = open(services);
    await user.click(await enabledButton('Add subscription'));
    const dialog = await screen.findByRole('dialog', { name: 'New event subscription' });
    // Remove the default event: none is left.
    await user.click(within(dialog).getByRole('textbox', { name: 'Events' }));
    await user.keyboard('{Backspace}');
    await user.keyboard('{Escape}');
    await user.click(within(dialog).getByRole('button', { name: 'Save subscription' }));
    expect(within(dialog).getByText('Choose at least one event.')).toBeInTheDocument();
    expect(set).not.toHaveBeenCalled();
  });

  it('shows the state of the targets and sends a test event', async () => {
    const { user } = open();
    const targets = await screen.findByRole('table', { name: 'Notification targets' });
    expect(within(targets).getByText('_:webhook')).toBeInTheDocument();
    expect(within(targets).getByText('Online')).toBeInTheDocument();
    expect(within(targets).getByText('Offline')).toBeInTheDocument();
    expect(within(targets).getByText('12')).toBeInTheDocument();

    await user.click(
      within(targets).getByRole('button', { name: 'Send a test event to _:webhook' }),
    );
    expect(
      await within(targets).findByText('Test event delivered to _:webhook.'),
    ).toBeInTheDocument();
    await user.click(
      within(targets).getByRole('button', { name: 'Send a test event to orders:kafka' }),
    );
    expect(
      await within(targets).findByText(
        'The test event could not be delivered to orders:kafka: dial tcp kafka-1.example.com:9092: connection refused',
      ),
    ).toBeInTheDocument();
  });

  it('deletes a subscription', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    await services.notifications.set('media', {
      subscriptions: [
        { id: 'thumbs', arn: 'arn:nexo:sqs:us-east-1:_:webhook', events: ['s3:ObjectCreated:*'] },
      ],
    });
    const { user } = open(services);
    await user.click(await screen.findByRole('button', { name: 'Delete subscription thumbs' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete the subscription' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(async () => {
      expect((await services.notifications.get('media')).subscriptions).toEqual([]);
    });
    expect(await screen.findByText('No subscription.')).toBeInTheDocument();
  });
});
