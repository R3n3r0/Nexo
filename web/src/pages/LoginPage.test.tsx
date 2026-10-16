import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Outlet, type RouteObject } from 'react-router';
import { describe, expect, it } from 'vitest';

import { AuthGuard } from '@/auth/AuthGuard';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { LoginPage } from './LoginPage';

const accounts = { admin: 'supersecret1' };

function routes(): RouteObject[] {
  return [
    { path: '/login', element: <LoginPage /> },
    {
      element: <AuthGuard />,
      children: [
        {
          element: <Outlet />,
          children: [
            { path: '/', element: <div>overview page</div> },
            { path: '/buckets', element: <div>buckets page</div> },
          ],
        },
      ],
    },
  ];
}

async function fillAndSubmit(accessKey: string, secretKey: string) {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText(/access key/i), accessKey);
  await user.type(screen.getByLabelText(/secret key/i), secretKey);
  await user.click(screen.getByRole('button', { name: /sign in/i }));
  return user;
}

describe('LoginPage', () => {
  it('shows the license and the source code before sign-in (AGPL-3.0 §13)', () => {
    renderRoutes(routes(), {
      initialEntries: ['/login'],
      services: createMockServices({ accounts }),
    });
    expect(screen.getByRole('link', { name: 'AGPL-3.0' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Powered by Nexo' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Source code' })).toHaveAttribute(
      'href',
      'https://github.com/R3n3r0/Nexo',
    );
  });

  it('keeps the submit button disabled until the form is valid', async () => {
    const user = userEvent.setup();
    renderRoutes(routes(), {
      initialEntries: ['/login'],
      services: createMockServices({ accounts }),
    });
    const submit = await screen.findByRole('button', { name: /sign in/i });
    expect(submit).toBeDisabled();

    await user.type(screen.getByLabelText(/access key/i), 'ad');
    await user.type(screen.getByLabelText(/secret key/i), 'short');
    expect(submit).toBeDisabled();

    await user.clear(screen.getByLabelText(/access key/i));
    await user.type(screen.getByLabelText(/access key/i), 'admin');
    await user.clear(screen.getByLabelText(/secret key/i));
    await user.type(screen.getByLabelText(/secret key/i), 'supersecret1');
    expect(submit).toBeEnabled();
  });

  it('shows validation messages after a field is left', async () => {
    const user = userEvent.setup();
    renderRoutes(routes(), {
      initialEntries: ['/login'],
      services: createMockServices({ accounts }),
    });
    const accessKey = await screen.findByLabelText(/access key/i);
    await user.click(accessKey);
    await user.tab();
    expect(screen.getByText('Enter your access key.')).toBeInTheDocument();
    await user.type(screen.getByLabelText(/secret key/i), 'short');
    await user.tab();
    expect(screen.getByText('The secret key must be at least 8 characters.')).toBeInTheDocument();
  });

  it('signs in and lands on the overview', async () => {
    const { router } = renderRoutes(routes(), {
      initialEntries: ['/login'],
      services: createMockServices({ accounts }),
    });
    await screen.findByRole('button', { name: /sign in/i });
    await fillAndSubmit('admin', 'supersecret1');
    expect(await screen.findByText('overview page')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
  });

  it('returns to the route that required authentication', async () => {
    const { router } = renderRoutes(routes(), {
      initialEntries: ['/buckets'],
      services: createMockServices({ accounts }),
    });
    // AuthGuard sends us to the login page first.
    await screen.findByRole('button', { name: /sign in/i });
    expect(router.state.location.pathname).toBe('/login');
    await fillAndSubmit('admin', 'supersecret1');
    expect(await screen.findByText('buckets page')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/buckets');
  });

  it('reports invalid credentials and stays on the page', async () => {
    const { router } = renderRoutes(routes(), {
      initialEntries: ['/login'],
      services: createMockServices({ accounts }),
    });
    await screen.findByRole('button', { name: /sign in/i });
    await fillAndSubmit('admin', 'wrong-password');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Invalid access key or secret key.');
    expect(router.state.location.pathname).toBe('/login');
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /sign in/i })).toBeEnabled();
    });
  });

  it('reports the throttling of repeated failures', async () => {
    renderRoutes(routes(), {
      initialEntries: ['/login'],
      services: createMockServices({ accounts }),
    });
    await screen.findByRole('button', { name: /sign in/i });
    const user = userEvent.setup();
    const secret = screen.getByLabelText(/secret key/i);
    await user.type(screen.getByLabelText(/access key/i), 'admin');
    for (let attempt = 0; attempt < 5; attempt++) {
      await user.clear(secret);
      await user.type(secret, `wrong-secret-${attempt}`);
      await user.click(screen.getByRole('button', { name: /sign in/i }));
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Invalid access key or secret key.',
      );
    }
    // Locked out: even the right secret is refused for now.
    await user.clear(secret);
    await user.type(secret, 'supersecret1');
    await user.click(screen.getByRole('button', { name: /sign in/i }));
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(
        'Too many failed attempts: wait a minute before signing in again.',
      );
    });
  });

  it('reports other failures with their message', async () => {
    const services = createMockServices({ accounts });
    const failing = {
      ...services,
      session: {
        ...services.session,
        login: () => Promise.reject(new Error('connection refused')),
      },
    };
    renderRoutes(routes(), { initialEntries: ['/login'], services: failing });
    await screen.findByRole('button', { name: /sign in/i });
    await fillAndSubmit('admin', 'supersecret1');
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Sign-in failed: connection refused',
    );
  });

  it('redirects an already authenticated user away from the login page', async () => {
    const { router } = renderRoutes(routes(), {
      initialEntries: ['/login'],
      services: createMockServices({ accounts, initialAccessKey: 'admin' }),
    });
    expect(await screen.findByText('overview page')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
  });

  it('shows the demo hint when demo credentials are provided', async () => {
    renderRoutes(
      [
        {
          path: '/login',
          element: (
            <LoginPage demoCredentials={{ accessKey: 'nexoadmin', secretKey: 'nexoadmin' }} />
          ),
        },
      ],
      {
        initialEntries: ['/login'],
      },
    );
    expect(
      await screen.findByText('Demo mode: sign in with nexoadmin / nexoadmin.'),
    ).toBeInTheDocument();
  });
});
