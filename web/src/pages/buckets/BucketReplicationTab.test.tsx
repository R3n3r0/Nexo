import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { BucketReplicationTab } from './BucketReplicationTab';

const routes = [{ path: '/buckets/:bucket/replication', element: <BucketReplicationTab /> }];

function open(
  bucket = 'backups',
  services = createMockServices({ initialAccessKey: 'nexoadmin' }),
) {
  const result = renderRoutes(routes, {
    initialEntries: [`/buckets/${bucket}/replication`],
    services,
  });
  return { ...result, user: userEvent.setup() };
}

async function enabledButton(name: string) {
  await waitFor(() => {
    expect(screen.getByRole('button', { name })).toBeEnabled();
  });
  return screen.getByRole('button', { name });
}

// covers: D-BKT-05
/** The replication of a bucket (D-BKT-05, S-BKT-12). */
describe('BucketReplicationTab', () => {
  it('adds a target and a rule, then resyncs the existing objects', async () => {
    const { user, services } = open();
    expect(await screen.findByText('Add a replication target first.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add rule' })).toBeDisabled();

    await user.click(await enabledButton('Add target'));
    let dialog = await screen.findByRole('dialog', { name: 'Add target' });
    await user.type(
      within(dialog).getByRole('textbox', { name: /^Endpoint/ }),
      'https://dr.example.com:9000/',
    );
    await user.type(within(dialog).getByRole('textbox', { name: /^Remote bucket/ }), 'backups');
    await user.type(within(dialog).getByRole('textbox', { name: /^Access key/ }), 'dr-key');
    await user.type(within(dialog).getByLabelText(/^Secret key/), 'dr-secret');
    await user.type(within(dialog).getByRole('textbox', { name: 'Label' }), 'dr-site');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const targets = await screen.findByRole('table', { name: 'Replication targets' });
    expect(within(targets).getByText('https://dr.example.com:9000/backups')).toBeInTheDocument();
    const [stored] = await services.replication.targets('backups');
    expect(stored).toMatchObject({ endpoint: 'https://dr.example.com:9000', label: 'dr-site' });
    expect(stored?.secretKey).toBeUndefined();

    await user.click(await enabledButton('Add rule'));
    dialog = await screen.findByRole('dialog', { name: 'Add rule' });
    await user.type(within(dialog).getByRole('textbox', { name: /^Rule ID/ }), 'docs');
    await user.type(within(dialog).getByRole('textbox', { name: /^Key prefix/ }), 'docs/');
    await user.type(
      within(dialog).getByRole('textbox', { name: /^Object tags/ }),
      'class=public{Enter}',
    );
    await user.click(within(dialog).getByRole('switch', { name: /^Replicate existing objects/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const rules = screen.getByRole('table', { name: 'Replication rules' });
    expect(within(rules).getByText('docs')).toBeInTheDocument();
    expect(within(rules).getByText('Prefix docs/, class=public')).toBeInTheDocument();
    expect(within(rules).getByText('Existing')).toBeInTheDocument();
    expect((await services.replication.get('backups')).rules).toEqual([
      {
        id: 'docs',
        priority: 1,
        enabled: true,
        target: stored?.arn,
        prefix: 'docs/',
        tags: { class: 'public' },
        deleteMarkers: true,
        deletes: false,
        existing: true,
      },
    ]);

    // A target the rules use cannot be removed.
    await user.click(screen.getByRole('button', { name: 'Remove the target dr-site' }));
    dialog = await screen.findByRole('dialog', { name: 'Remove the replication target' });
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }));
    expect(
      await within(dialog).findByText(/the replication rules use the target/),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));

    await user.click(await enabledButton('Resync'));
    expect(await screen.findByText(/Resync to dr-site/)).toBeInTheDocument();
  });

  it('refuses a rule on a bucket without versioning', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    await services.replication.setTarget('media', {
      endpoint: 'https://dr.example.com',
      bucket: 'media',
      accessKey: 'k',
      secretKey: 's',
    });
    const { user } = open('media', services);
    await user.click(await enabledButton('Add rule'));
    const dialog = await screen.findByRole('dialog', { name: 'Add rule' });
    await user.type(within(dialog).getByRole('textbox', { name: /^Rule ID/ }), 'all');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/versioning/);
  });
});
