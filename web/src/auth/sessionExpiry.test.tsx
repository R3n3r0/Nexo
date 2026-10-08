import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Outlet, type RouteObject } from 'react-router';
import { describe, expect, it } from 'vitest';

import { LoginPage } from '@/pages/LoginPage';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { AuthGuard } from './AuthGuard';

const routes: RouteObject[] = [
  { path: '/login', element: <LoginPage /> },
  {
    element: <AuthGuard />,
    children: [
      {
        element: <Outlet />,
        children: [{ path: '/policies/:name', element: <div>policy page</div> }],
      },
    ],
  },
];

describe('session expiry (TD-U-02, TD-E-17)', () => {
  it('sends the user to the login page with a message and back to the same route', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const { router } = renderRoutes(routes, {
      initialEntries: ['/policies/readonly?tab=builder'],
      services,
    });
    expect(await screen.findByText('policy page')).toBeInTheDocument();

    services.session.expire();
    await act(async () => {
      await services.session.current().catch(() => undefined);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/login');
    });
    expect(screen.getByRole('alert')).toHaveTextContent('Your session has expired');

    await user.type(screen.getByLabelText(/access key/i), 'nexoadmin');
    await user.type(screen.getByLabelText(/secret key/i), 'nexoadmin');
    await user.click(screen.getByRole('button', { name: /sign in/i }));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/policies/readonly');
    });
    expect(router.state.location.search).toBe('?tab=builder');
  });

  it('shows no expiry message to a user who was never signed in', async () => {
    renderRoutes(routes, { initialEntries: ['/policies/readonly'] });
    expect(await screen.findByRole('button', { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByText(/session has expired/)).not.toBeInTheDocument();
  });
});
