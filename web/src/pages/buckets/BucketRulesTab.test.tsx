import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { ApiError, type BucketAccessRule } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { BucketRulesTab } from './BucketRulesTab';

const routes = [{ path: '/buckets/:bucket/rules', element: <BucketRulesTab /> }];

function admin() {
  return createMockServices({
    initialAccessKey: 'nexoadmin',
    objects: {
      media: [
        { key: 'img/2026/a.png' },
        { key: 'img/2026/b.JPG' },
        { key: 'img/old/c.png' },
        { key: 'docs/readme.txt' },
      ],
    },
    buckets: [
      {
        name: 'media',
        created: '2026-09-12T15:30:00Z',
        region: 'us-east-1',
        versioning: 'off',
        objectLockEnabled: false,
        objects: 4,
        versions: 0,
        size: 10,
        quotaBytes: 0,
        tags: {},
      },
    ],
  });
}

function open(services = admin()) {
  const result = renderRoutes(routes, { initialEntries: ['/buckets/media/rules'], services });
  return { ...result, user: userEvent.setup() };
}

const rule = (
  id: string,
  priority: number,
  extra: Partial<BucketAccessRule> = {},
): BucketAccessRule => ({
  id,
  priority,
  effect: 'allow',
  principals: ['user:mario'],
  actions: ['read'],
  ...extra,
});

async function addRuleButton() {
  await waitFor(() => {
    expect(screen.getByRole('button', { name: 'Add rule' })).toBeEnabled();
  });
  return screen.getByRole('button', { name: 'Add rule' });
}

/** D-ACC-01..04 / TD-A-01..03 on the mocks. */
describe('BucketRulesTab', () => {
  it('TD-A-01 creates a regex rule whose preview highlights the matching keys', async () => {
    const { user, services } = open();
    expect(await screen.findByText(/No access rule/)).toBeInTheDocument();
    await user.click(await addRuleButton());
    const dialog = await screen.findByRole('dialog', { name: 'New access rule' });
    const id = within(dialog).getByRole('textbox', { name: 'Rule ID' });
    await user.clear(id);
    await user.type(id, 'img-pub');
    await user.click(within(dialog).getByRole('textbox', { name: 'Pattern type' }));
    await user.click(await screen.findByRole('option', { name: 'Regular expression' }));
    const pattern = within(dialog).getByRole('textbox', { name: 'Pattern' });
    await user.click(pattern);
    await user.paste('^img/[0-9]{4}/.*\\.(jpe?g|png)$');
    await user.click(within(dialog).getByRole('switch', { name: 'Ignore case' }));

    // The sample keys of the bucket are matched live.
    const list = await within(dialog).findByRole('list', {
      name: 'Sample keys and whether the pattern matches them',
    });
    await waitFor(() => {
      expect(within(list).getByLabelText('img/2026/a.png: matches')).toBeInTheDocument();
    });
    expect(within(list).getByLabelText('img/2026/b.JPG: matches')).toBeInTheDocument();
    expect(within(list).getByLabelText('img/old/c.png: does not match')).toBeInTheDocument();
    expect(within(list).getByLabelText('docs/readme.txt: does not match')).toBeInTheDocument();
    expect(within(dialog).getByText('2 of 4 keys match.')).toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const table = screen.getByRole('table', { name: 'Access rules of the bucket' });
    expect(within(table).getByText('img-pub')).toBeInTheDocument();
    expect((await services.accessRules.get('media')).rules).toEqual([
      {
        id: 'img-pub',
        priority: 10,
        effect: 'allow',
        principals: ['*'],
        actions: ['read'],
        match: { type: 'regex', pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png)$', caseInsensitive: true },
      },
    ]);
    // The bucket is public now.
    expect(
      within(screen.getByRole('region', { name: 'Access rules' })).getByText('Public'),
    ).toBeInTheDocument();
  });

  it('TD-A-01 blocks an invalid regex with its position and shows the server position too', async () => {
    const services = admin();
    const set = vi.spyOn(services.accessRules, 'set');
    const { user } = open(services);
    await user.click(await addRuleButton());
    const dialog = await screen.findByRole('dialog', { name: 'New access rule' });
    await user.click(within(dialog).getByRole('textbox', { name: 'Pattern type' }));
    await user.click(await screen.findByRole('option', { name: 'Regular expression' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Pattern' }), 'a(?=b)');
    expect(
      within(dialog).getByText('Position 2: lookaround assertions are not supported (RE2)'),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(within(dialog).getByText('Fix the highlighted fields.')).toBeInTheDocument();
    expect(set).not.toHaveBeenCalled();

    // An error found by the server only (here RE2-specific) is located too.
    set.mockRejectedValueOnce(
      new ApiError({
        title: 'Bad Request',
        status: 400,
        code: 'invalid_access_rule',
        detail: 'bad',
        accessRuleErrors: [
          {
            rule: 1,
            id: 'rule-1',
            field: 'match.pattern',
            position: 3,
            message: 'invalid escape \\C',
          },
        ],
      }),
    );
    const input = within(dialog).getByRole('textbox', { name: 'Pattern' });
    await user.clear(input);
    await user.type(input, 'ab\\d');
    await user.click(within(dialog).getByRole('button', { name: 'Save rule' }));
    expect(await within(dialog).findByText('Position 3: invalid escape \\C')).toBeInTheDocument();
  });

  it('TD-A-02 reorders the rules and saves the priorities with If-Match', async () => {
    const services = admin();
    const first = await services.accessRules.set('media', [
      rule('a', 10),
      rule('b', 20),
      rule('c', 30),
    ]);
    const set = vi.spyOn(services.accessRules, 'set');
    const { user } = open(services);
    const table = await screen.findByRole('table', { name: 'Access rules of the bucket' });
    await waitFor(() => {
      expect(within(table).getByRole('button', { name: 'Move c up' })).toBeEnabled();
    });
    await user.click(within(table).getByRole('button', { name: 'Move c up' }));
    await waitFor(() => {
      expect(set).toHaveBeenCalledTimes(1);
    });
    expect(set.mock.calls[0]?.[2]).toBe(first.etag);
    const stored = await services.accessRules.get('media');
    expect([...stored.rules].sort((x, y) => x.priority - y.priority).map((r) => r.id)).toEqual([
      'a',
      'c',
      'b',
    ]);
    await waitFor(() => {
      const ids = within(screen.getByRole('table', { name: 'Access rules of the bucket' }))
        .getAllByRole('row')
        .slice(1)
        .map((r) => r.getAttribute('data-rule'));
      expect(ids).toEqual(['a', 'c', 'b']);
    });
  });

  it('TD-A-02 reports a concurrent change (412) and reloads the rules', async () => {
    const services = admin();
    await services.accessRules.set('media', [rule('a', 10), rule('b', 20)]);
    const { user } = open(services);
    const table = await screen.findByRole('table', { name: 'Access rules of the bucket' });
    // Someone else saves meanwhile.
    await services.accessRules.set('media', [rule('a', 10), rule('b', 20), rule('z', 5)]);
    await waitFor(() => {
      expect(within(table).getByRole('button', { name: 'Move b up' })).toBeEnabled();
    });
    await user.click(within(table).getByRole('button', { name: 'Move b up' }));
    expect(await screen.findByText('The rules changed')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Reload the rules' }));
    await waitFor(() => {
      expect(screen.queryByText('The rules changed')).not.toBeInTheDocument();
    });
    expect(
      await within(screen.getByRole('table', { name: 'Access rules of the bucket' })).findByText(
        'z',
      ),
    ).toBeInTheDocument();
  });

  it('TD-A-03 applies the public-prefix preset and shows the public state', async () => {
    const { user, services } = open();
    const panel = await screen.findByRole('region', { name: 'Public access' });
    await user.click(within(panel).getByRole('radio', { name: /Public read of a prefix/ }));
    const pattern = within(panel).getByRole('textbox', { name: 'Public keys' });
    await user.clear(pattern);
    await user.type(pattern, 'img/');
    await waitFor(() => {
      expect(
        within(panel).getByRole('button', { name: 'Apply “Public read of a prefix”' }),
      ).toBeEnabled();
    });
    await user.click(
      within(panel).getByRole('button', { name: 'Apply “Public read of a prefix”' }),
    );
    await waitFor(async () => {
      expect(await services.accessRules.publicStatus('media')).toMatchObject({
        public: true,
        preset: 'public-prefix',
      });
    });
    expect(await within(panel).findByText('Public')).toBeInTheDocument();
    expect(
      within(screen.getByRole('table', { name: 'Access rules of the bucket' })).getByText(
        'preset-public-prefix',
      ),
    ).toBeInTheDocument();
    expect((await services.buckets.get('media')).public).toBe(true);
  });

  it('TD-A-03 needs an explicit confirmation for public-write', async () => {
    const { user } = open();
    const panel = await screen.findByRole('region', { name: 'Public access' });
    await user.click(within(panel).getByRole('radio', { name: /Public write/ }));
    expect(within(panel).getByText(/Anyone on the network can upload/)).toBeInTheDocument();
    const apply = within(panel).getByRole('button', { name: 'Apply “Public write”' });
    expect(apply).toBeDisabled();
    await user.click(
      within(panel).getByRole('checkbox', {
        name: 'I understand that anyone will be able to upload to this bucket',
      }),
    );
    expect(apply).toBeEnabled();
  });

  it('D-ACC-02 tests keys and highlights the rules that decide', async () => {
    const services = admin();
    await services.accessRules.set('media', [
      rule('public-img', 10, { principals: ['*'], match: { type: 'prefix', pattern: 'img/' } }),
      rule('no-old', 5, {
        effect: 'deny',
        principals: ['*'],
        actions: ['all'],
        match: { type: 'glob', pattern: '**/old/**' },
      }),
    ]);
    const { user } = open(services);
    const tester = await screen.findByRole('region', { name: 'Pattern tester' });
    await user.click(within(tester).getByRole('button', { name: 'Suggest keys of the bucket' }));
    await waitFor(() => {
      expect(within(tester).getByRole('textbox', { name: 'Keys' })).toHaveValue(
        'docs/readme.txt\nimg/2026/a.png\nimg/2026/b.JPG\nimg/old/c.png',
      );
    });
    await user.click(within(tester).getByRole('button', { name: 'Test' }));
    const results = await within(tester).findByRole('list', { name: 'Results of the test' });
    const old = within(results).getByText('img/old/c.png').closest('[data-key]');
    expect(old).not.toBeNull();
    expect(within(old as HTMLElement).getByText('Denied')).toBeInTheDocument();
    expect(
      within(old as HTMLElement).getByText('Decided by the rule no-old.', { exact: false }),
    ).toBeInTheDocument();
    await user.click(
      within(results).getByRole('button', {
        name: 'Highlight in the table the rules of img/old/c.png',
      }),
    );
    const table = screen.getByRole('table', { name: 'Access rules of the bucket' });
    expect(within(table).getByText('Decides')).toBeInTheDocument();
  });

  it('shows the rules as YAML and locates a syntax error', async () => {
    const services = admin();
    await services.accessRules.set('media', [
      rule('a', 10, { match: { type: 'glob', pattern: '**/x/**' } }),
    ]);
    const { user } = open(services);
    await screen.findByRole('table', { name: 'Access rules of the bucket' });
    await user.click(screen.getByText('YAML'));
    const editor = await screen.findByRole('textbox', { name: 'Rules (YAML)' });
    expect((editor as HTMLTextAreaElement).value).toContain("pattern: '**/x/**'");
    await user.clear(editor);
    await user.click(editor);
    await user.paste('rules: [{ id: a');
    expect(await screen.findByText(/unterminated flow/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save rules' })).toBeDisabled();
  });

  it('is not available without admin:ManageAccessRules', async () => {
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
    open(services);
    expect(
      await screen.findByText('You may not manage the access rules of this bucket.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add rule' })).not.toBeInTheDocument();
  });
});
