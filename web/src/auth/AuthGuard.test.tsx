import { act, screen, waitFor } from '@testing-library/react';
import { Outlet, useLocation, type RouteObject } from 'react-router';
import { describe, expect, it } from 'vitest';

import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { AuthGuard } from './AuthGuard';

function LoginProbe() {
  const location = useLocation();
  const from = (location.state as { from?: string } | null)?.from ?? '';
  return <div>login page (from: {from})</div>;
}

const routes: RouteObject[] = [
  { path: '/login', element: <LoginProbe /> },
  {
    element: <AuthGuard />,
    children: [
      {
        element: <Outlet />,
        children: [
          { path: '/', element: <div>protected overview</div> },
          { path: '/buckets', element: <div>protected buckets</div> },
        ],
      },
    ],
  },
];

describe('AuthGuard', () => {
  it('shows a loader while the session is being resolved', () => {
    renderRoutes(routes, { services: createMockServices({ latencyMs: 1000 }) });
    expect(screen.getByTestId('auth-loading')).toBeInTheDocument();
  });

  it('redirects anonymous users to /login remembering the requested route', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets?page=2'] });
    expect(await screen.findByText('login page (from: /buckets?page=2)')).toBeInTheDocument();
  });

  it('renders the protected content for an authenticated session', async () => {
    renderRoutes(routes, {
      initialEntries: ['/buckets'],
      services: createMockServices({ initialAccessKey: 'nexoadmin' }),
    });
    expect(await screen.findByText('protected buckets')).toBeInTheDocument();
  });

  it('sends the user back to /login when any call answers 401', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const { router } = renderRoutes(routes, { initialEntries: ['/'], services });
    expect(await screen.findByText('protected overview')).toBeInTheDocument();

    // The session expires server-side; the next call raises the global event.
    services.session.expire();
    await act(async () => {
      await services.session.current().catch(() => undefined);
    });

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/login');
    });
    expect(screen.getByText('login page (from: /)')).toBeInTheDocument();
  });
});
