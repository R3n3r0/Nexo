import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { ApiError } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { BucketLifecycleTab } from './BucketLifecycleTab';

const routes = [{ path: '/buckets/:bucket/lifecycle', element: <BucketLifecycleTab /> }];

function admin() {
  return createMockServices({ initialAccessKey: 'nexoadmin' });
}

function open(services = admin()) {
  const result = renderRoutes(routes, {
    initialEntries: ['/buckets/backups/lifecycle'],
    services,
  });
  return { ...result, user: userEvent.setup() };
}

/** The "Add rule" button, once the permissions allow it (it is re-rendered then). */
async function addRuleButton() {
  await waitFor(() => {
    expect(screen.getByRole('button', { name: 'Add rule' })).toBeEnabled();
  });
  return screen.getByRole('button', { name: 'Add rule' });
}

/** D-BKT-03 / TD-U-08: the lifecycle tab, its guided editor and its code views. */
describe('BucketLifecycleTab', () => {
  it('creates a rule with the guided form and lists it', async () => {
    const { user, services } = open();
    expect(
      await screen.findByText('No lifecycle rule: the objects are kept until they are deleted.'),
    ).toBeInTheDocument();
    await user.click(await addRuleButton());
    const dialog = await screen.findByRole('dialog', { name: 'New lifecycle rule' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Rule ID' }), 'logs');
    await user.type(within(dialog).getByRole('textbox', { name: 'Prefix' }), 'logs/');
    const days = within(dialog).getByRole('textbox', { name: 'Days after the creation' });
    await user.clear(days);
    await user.type(days, '45');
    await user.click(within(dialog).getByRole('switch', { name: 'Abort the incomplete uploads' }));
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const table = screen.getByRole('table', { name: 'Lifecycle rules' });
    expect(within(table).getByText('logs')).toBeInTheDocument();
    expect(within(table).getByText('Prefix logs/')).toBeInTheDocument();
    expect(within(table).getByText('Current versions expire after 45 days')).toBeInTheDocument();
    expect(
      within(table).getByText('Incomplete uploads are aborted after 7 days'),
    ).toBeInTheDocument();
    expect(await services.lifecycle.get('backups')).toEqual({
      rules: [
        {
          id: 'logs',
          enabled: true,
          filter: { prefix: 'logs/' },
          expiration: { days: 45 },
          abortIncompleteUpload: { daysAfterInitiation: 7 },
        },
      ],
    });
  });

  it('validates the form before sending: days > 0, an action, filters', async () => {
    const services = admin();
    const set = vi.spyOn(services.lifecycle, 'set');
    const { user } = open(services);
    await user.click(await addRuleButton());
    const dialog = await screen.findByRole('dialog', { name: 'New lifecycle rule' });
    const days = within(dialog).getByRole('textbox', { name: 'Days after the creation' });
    await user.clear(days);
    await user.type(days, '0');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(within(dialog).getByText('Enter a whole number of at least 1.')).toBeInTheDocument();

    await user.click(within(dialog).getByRole('textbox', { name: 'Expiration' }));
    await user.click(await screen.findByRole('option', { name: 'Never' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Choose at least one action.');

    await user.click(within(dialog).getByRole('button', { name: 'Add tag' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Key of tag 1' }), 'team');
    await user.click(within(dialog).getByRole('switch', { name: 'Abort the incomplete uploads' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent(
      'Incomplete uploads cannot be filtered by tags or size.',
    );
    expect(set).not.toHaveBeenCalled();
  });

  it('keeps the form and the JSON and XML views of a rule in sync', async () => {
    const { user } = open();
    await user.click(await addRuleButton());
    const dialog = await screen.findByRole('dialog', { name: 'New lifecycle rule' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Prefix' }), 'tmp/');

    // Form -> XML.
    await user.click(within(dialog).getByRole('radio', { name: 'XML' }));
    const xml = await within(dialog).findByRole('textbox', { name: 'Rule (S3 XML)' });
    expect(xml).toHaveValue(
      [
        '<Rule>',
        '  <Filter>',
        '    <Prefix>tmp/</Prefix>',
        '  </Filter>',
        '  <Status>Enabled</Status>',
        '  <Expiration>',
        '    <Days>30</Days>',
        '  </Expiration>',
        '</Rule>',
      ].join('\n'),
    );
    // XML -> form: a valid edit updates the rule, an invalid one is reported.
    await user.clear(xml);
    await user.click(xml);
    await user.paste(
      '<Rule><ID>tmp</ID><Status>Disabled</Status><Expiration><Days>3</Days></Expiration></Rule>',
    );
    await user.click(within(dialog).getByRole('radio', { name: 'JSON' }));
    const json = await within(dialog).findByRole('textbox', { name: 'Rule (JSON)' });
    expect(JSON.parse((json as HTMLTextAreaElement).value)).toEqual({
      id: 'tmp',
      enabled: false,
      expiration: { days: 3 },
    });
    await user.clear(json);
    await user.click(json);
    await user.paste('{"id": "tmp", "expiration": {"days": "x"}}');
    expect(
      within(dialog).getByText('"days" must be a number', { exact: false }),
    ).toBeInTheDocument();

    await user.click(within(dialog).getByRole('radio', { name: 'Form' }));
    expect(within(dialog).getByRole('textbox', { name: 'Rule ID' })).toHaveValue('tmp');
    expect(within(dialog).getByRole('switch', { name: 'Enabled' })).not.toBeChecked();
    expect(within(dialog).getByRole('textbox', { name: 'Days after the creation' })).toHaveValue(
      '3',
    );
  });

  it('shows the problems of the server for the rule it refused', async () => {
    const services = admin();
    vi.spyOn(services.lifecycle, 'set').mockRejectedValue(
      new ApiError({
        title: 'Bad Request',
        status: 400,
        code: 'invalid_configuration',
        detail: 'invalid',
        ruleErrors: [
          { rule: 1, id: 'x', code: 'InvalidArgument', message: "'Date' must be at midnight GMT" },
        ],
      }),
    );
    const { user } = open(services);
    await user.click(await addRuleButton());
    const dialog = await screen.findByRole('dialog', { name: 'New lifecycle rule' });
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(
      await within(dialog).findByText("'Date' must be at midnight GMT (InvalidArgument)"),
    ).toBeInTheDocument();
    expect(within(dialog).getByText('The server refused the configuration')).toBeInTheDocument();
  });

  it('edits the whole configuration as S3 XML and applies the rules now', async () => {
    const services = admin();
    await services.lifecycle.set('backups', {
      rules: [{ id: 'old', enabled: true, expiration: { days: 1 } }],
    });
    const apply = vi.spyOn(services.lifecycle, 'apply');
    const { user } = open(services);
    expect(await screen.findByText('old')).toBeInTheDocument();
    await user.click(screen.getByRole('radio', { name: 'XML' }));
    const xml = await screen.findByRole('textbox', { name: 'LifecycleConfiguration (S3 XML)' });
    expect((xml as HTMLTextAreaElement).value).toContain('<ID>old</ID>');
    await user.clear(xml);
    await user.click(xml);
    await user.paste(
      '<LifecycleConfiguration><Rule><ID>new</ID><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>5</NoncurrentDays></NoncurrentVersionExpiration></Rule></LifecycleConfiguration>',
    );
    await user.click(screen.getByRole('button', { name: 'Save the configuration' }));
    await waitFor(async () => {
      expect((await services.lifecycle.get('backups')).rules.map((r) => r.id)).toEqual(['new']);
    });
    await user.click(screen.getByRole('radio', { name: 'Rules' }));
    expect(await screen.findByText('Noncurrent versions expire after 5 days')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Apply now' }));
    const confirm = await screen.findByRole('dialog', { name: 'Apply the lifecycle rules now' });
    await user.click(within(confirm).getByRole('button', { name: 'Apply now' }));
    await waitFor(() => {
      expect(apply).toHaveBeenCalledWith('backups');
    });
  });

  it('deletes a rule and toggles another', async () => {
    const services = admin();
    await services.lifecycle.set('backups', {
      rules: [
        { id: 'a', enabled: true, expiration: { days: 1 } },
        { id: 'b', enabled: true, expiration: { days: 2 } },
      ],
    });
    const { user } = open(services);
    await user.click(await screen.findByRole('switch', { name: 'Enable rule a' }));
    await waitFor(async () => {
      expect((await services.lifecycle.get('backups')).rules[0]?.enabled).toBe(false);
    });
    await user.click(screen.getByRole('button', { name: 'Delete rule b' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete the rule' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(async () => {
      expect((await services.lifecycle.get('backups')).rules.map((r) => r.id)).toEqual(['a']);
    });
  });

  it('offers no change to a read-only user', async () => {
    const services = createMockServices({
      accounts: { nexoadmin: 'nexoadmin', reader: 'reader-secret' },
      users: [
        {
          accessKey: 'reader',
          status: 'enabled',
          policyNames: ['readonly'],
          groups: [],
          createdAt: '2026-09-01T00:00:00Z',
          updatedAt: '2026-09-01T00:00:00Z',
        },
      ],
      groups: [],
      initialAccessKey: 'reader',
    });
    const { user } = open(services);
    await screen.findByText('No lifecycle rule: the objects are kept until they are deleted.');
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Add rule' })).toBeDisabled();
    });
    expect(screen.getByRole('button', { name: 'Apply now' })).toBeDisabled();
    await user.click(screen.getByRole('radio', { name: 'JSON' }));
    expect(
      screen.queryByRole('button', { name: 'Save the configuration' }),
    ).not.toBeInTheDocument();
  });
});
