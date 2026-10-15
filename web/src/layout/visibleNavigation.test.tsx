import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { createRoutes } from '@/router';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { navigation, visibleNavigation } from './navigation';

describe('visibleNavigation (D-UX-02)', () => {
  const keys = (items: ReturnType<typeof visibleNavigation>) => items.map((i) => i.key);

  it('shows every section until the permissions are known', () => {
    expect(keys(visibleNavigation(navigation, { ready: false, can: () => false }))).toEqual(
      navigation.map((i) => i.key),
    );
  });

  it('hides the sections whose action is denied', () => {
    const visible = keys(
      visibleNavigation(navigation, { ready: true, can: (a) => a === 'admin:ListUsers' }),
    );
    expect(visible).toContain('users');
    expect(visible).toContain('accessKeys');
    expect(visible).toContain('shares');
    expect(visible).not.toContain('groups');
    expect(visible).not.toContain('policies');
    expect(visible).not.toContain('simulator');
    expect(visible).not.toContain('configuration');
  });

  it('shows the cache to a user who may manage it or only invalidate it', () => {
    const visible = (actions: string[]) =>
      keys(visibleNavigation(navigation, { ready: true, can: (a) => actions.includes(a) }));
    expect(visible(['admin:InvalidateCache'])).toContain('cache');
    expect(visible(['admin:ManageCache'])).toContain('cache');
    expect(visible(['admin:ListUsers'])).not.toContain('cache');
  });

  it('hides the admin menu of a user without admin permissions', async () => {
    renderRoutes(createRoutes(), {
      initialEntries: ['/access-keys'],
      services: createMockServices({
        accounts: { nexoadmin: 'nexoadmin', alice: 'alice-secret' },
        initialAccessKey: 'alice',
      }),
    });
    const nav = await screen.findByRole('navigation');
    await waitFor(() => {
      expect(within(nav).queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
    });
    expect(within(nav).queryByRole('link', { name: 'Policies' })).not.toBeInTheDocument();
    expect(within(nav).queryByRole('link', { name: 'Configuration' })).not.toBeInTheDocument();
    expect(within(nav).getByRole('link', { name: 'Access keys' })).toBeInTheDocument();
    expect(within(nav).getByRole('link', { name: 'Shares' })).toBeInTheDocument();
  });

  it('shows the admin menu to the root user', async () => {
    renderRoutes(createRoutes(), {
      initialEntries: ['/policy-simulator'],
      services: createMockServices({ initialAccessKey: 'nexoadmin' }),
    });
    const nav = await screen.findByRole('navigation');
    expect(within(nav).getByRole('link', { name: 'Users' })).toBeInTheDocument();
    expect(within(nav).getByRole('link', { name: 'Policy simulator' })).toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: 'Policy simulator' })).toBeInTheDocument();
  });
});
