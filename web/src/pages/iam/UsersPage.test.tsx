import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { samplePolicies, sampleUsers } from '@/services/mock';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { GroupsPage } from './GroupsPage';
import { UsersPage } from './UsersPage';

const routes = [
  { path: '/users', element: <UsersPage /> },
  { path: '/groups', element: <GroupsPage /> },
  { path: '/access-keys', element: <div>access keys page</div> },
];

/** A user that may read users but change nothing. */
function viewerServices() {
  return createMockServices({
    accounts: { nexoadmin: 'nexoadmin', viewer: 'viewer-secret' },
    initialAccessKey: 'viewer',
    policies: [
      ...samplePolicies(),
      {
        name: 'user-viewer',
        builtIn: false,
        updatedAt: '2026-10-01T00:00:00Z',
        document: {
          Version: '2012-10-17',
          Statement: [
            {
              Effect: 'Allow',
              Action: [
                'admin:ListUsers',
                'admin:GetUser',
                'admin:ListGroups',
                'admin:ListUserPolicies',
              ],
            },
          ],
        },
      },
    ],
    users: [
      ...sampleUsers(),
      {
        accessKey: 'viewer',
        status: 'enabled',
        policyNames: ['user-viewer'],
        groups: [],
        createdAt: '2026-10-01T00:00:00Z',
        updatedAt: '2026-10-01T00:00:00Z',
      },
    ],
  });
}

// covers: D-IAM-01
describe('Users page (D-IAM-01)', () => {
  it('lists, searches, creates, disables and deletes users', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    renderRoutes(routes, { initialEntries: ['/users'], services });
    const table = await screen.findByRole('table', { name: 'Users' });
    expect(await within(table).findByRole('button', { name: 'alice' })).toBeInTheDocument();

    await user.type(screen.getByRole('textbox', { name: 'Search' }), 'ci');
    expect(within(table).queryByRole('button', { name: 'alice' })).not.toBeInTheDocument();
    expect(within(table).getByRole('button', { name: 'ci-bot' })).toBeInTheDocument();
    await user.clear(screen.getByRole('textbox', { name: 'Search' }));

    // Create: validation first, then generated credentials.
    await user.click(await screen.findByRole('button', { name: 'Create user' }));
    const form = await screen.findByRole('form', { name: 'Create user' });
    await user.type(within(form).getByRole('textbox', { name: /Access key/ }), 'x');
    await user.click(within(form).getByRole('button', { name: 'Create' }));
    expect(
      within(form).getByText('The access key must be 3 to 20 characters.'),
    ).toBeInTheDocument();
    expect(within(form).getByText('Enter a secret key.')).toBeInTheDocument();
    await user.clear(within(form).getByRole('textbox', { name: /Access key/ }));
    await user.type(within(form).getByRole('textbox', { name: /Access key/ }), 'dave');
    await user.click(within(form).getByRole('button', { name: 'Generate a secret key' }));
    await user.click(within(form).getByRole('button', { name: 'Create' }));
    expect(await within(table).findByRole('button', { name: 'dave' })).toBeInTheDocument();

    // Disable from the row menu.
    await user.click(screen.getByRole('button', { name: 'Actions for dave' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Disable' }));
    await waitFor(async () => {
      expect((await services.users.get('dave')).status).toBe('disabled');
    });

    // Delete with confirmation.
    await user.click(screen.getByRole('button', { name: 'Actions for dave' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Delete' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete user' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(() => {
      expect(within(table).queryByRole('button', { name: 'dave' })).not.toBeInTheDocument();
    });
  });

  it('shows the server error when the user already exists', async () => {
    const user = userEvent.setup();
    renderRoutes(routes, {
      initialEntries: ['/users'],
      services: createMockServices({ initialAccessKey: 'nexoadmin' }),
    });
    await user.click(await screen.findByRole('button', { name: 'Create user' }));
    const form = await screen.findByRole('form', { name: 'Create user' });
    await user.type(within(form).getByRole('textbox', { name: /Access key/ }), 'alice');
    await user.click(within(form).getByRole('button', { name: 'Generate a secret key' }));
    await user.click(within(form).getByRole('button', { name: 'Create' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('User alice already exists.');
  });

  it('assigns policies and groups and shows the effective permissions', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    renderRoutes(routes, { initialEntries: ['/users'], services });
    await user.click(await screen.findByRole('button', { name: 'ci-bot' }));
    const drawer = await screen.findByRole('dialog', { name: 'User ci-bot' });
    const groups = await within(drawer).findByRole('textbox', { name: 'Groups' });
    await user.click(groups);
    await user.click(await screen.findByRole('option', { name: 'developers' }));
    await user.click(within(drawer).getByRole('button', { name: 'Save' }));
    await waitFor(async () => {
      expect((await services.users.get('ci-bot')).groups).toEqual(['developers']);
    });

    await user.click(within(drawer).getByRole('tab', { name: 'Effective permissions' }));
    const allowed = await within(drawer).findByRole('region', { name: /Allowed/ });
    await waitFor(() => {
      expect(allowed).toHaveTextContent('s3:PutObject');
    });
    expect(within(drawer).getByRole('region', { name: /Denied/ })).toHaveTextContent(
      'admin:ListUsers',
    );
    expect(within(drawer).getByText('Through group developers:')).toBeInTheDocument();
  });
});

// covers: D-UX-02
describe('permission-aware UI (TD-U-09)', () => {
  it('hides create and delete and disables the controls the user may not use', async () => {
    const user = userEvent.setup();
    renderRoutes(routes, { initialEntries: ['/users'], services: viewerServices() });
    const table = await screen.findByRole('table', { name: 'Users' });
    expect(await within(table).findByRole('button', { name: 'alice' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Create user' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Actions for alice' })).not.toBeInTheDocument();

    await user.click(within(table).getByRole('button', { name: 'alice' }));
    const drawer = await screen.findByRole('dialog', { name: 'User alice' });
    expect(within(drawer).getByRole('switch', { name: 'Enabled' })).toBeDisabled();
    expect(
      within(drawer).getByLabelText('You do not have the permission admin:DisableUser.'),
    ).toBeInTheDocument();
    expect(
      within(drawer).getByLabelText(
        'You do not have the permission admin:AttachUserOrGroupPolicy.',
      ),
    ).toBeInTheDocument();
  });

  it('reports a 403 when the user may not list users', async () => {
    renderRoutes(routes, {
      initialEntries: ['/users'],
      services: createMockServices({
        accounts: { nexoadmin: 'nexoadmin', alice: 'alice-secret' },
        initialAccessKey: 'alice',
      }),
    });
    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not load the users/);
    expect(screen.getByRole('alert')).toHaveTextContent(/admin:ListUsers is not allowed/);
  });
});

// covers: D-IAM-02
describe('Groups page (D-IAM-02)', () => {
  it('creates a group with members and edits members, policies and status', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    renderRoutes(routes, { initialEntries: ['/groups'], services });
    await user.click(await screen.findByRole('button', { name: 'Create group' }));
    const form = await screen.findByRole('form', { name: 'Create group' });
    await user.click(within(form).getByRole('button', { name: 'Create' }));
    expect(within(form).getByText('Enter a group name.')).toBeInTheDocument();
    await user.type(within(form).getByRole('textbox', { name: 'Name' }), 'qa');
    await user.click(within(form).getByRole('textbox', { name: 'Members' }));
    await user.click(await screen.findByRole('option', { name: 'ci-bot' }));
    await user.click(within(form).getByRole('button', { name: 'Create' }));

    const table = await screen.findByRole('table', { name: 'Groups' });
    await user.click(await within(table).findByRole('button', { name: 'qa' }));
    const drawer = await screen.findByRole('dialog', { name: 'Group qa' });
    await user.click(within(drawer).getByRole('switch', { name: 'Enabled' }));
    await user.click(within(drawer).getByRole('textbox', { name: 'Policies' }));
    await user.click(await screen.findByRole('option', { name: 'readonly' }));
    await user.click(within(drawer).getByRole('button', { name: 'Save' }));
    await waitFor(async () => {
      expect(await services.groups.get('qa')).toMatchObject({
        status: 'disabled',
        members: ['ci-bot'],
        policyNames: ['readonly'],
      });
    });
    expect((await services.users.get('ci-bot')).groups).toContain('qa');
  });
});
