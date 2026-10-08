import { Center, Loader } from '@mantine/core';
import type { ReactNode } from 'react';
import { Navigate, Outlet, useLocation } from 'react-router';

import { useSession } from './SessionProvider';

export interface AuthGuardProps {
  /** Rendered when authenticated; defaults to the nested routes (<Outlet />). */
  children?: ReactNode;
}

/**
 * Protects the routes below it: while the session is being resolved it
 * shows a loader, without a session it redirects to /login remembering the
 * requested route (and whether the session expired while in use),
 * otherwise it renders the children.
 */
export function AuthGuard({ children }: AuthGuardProps) {
  const { status, expired } = useSession();
  const location = useLocation();

  if (status === 'loading') {
    return (
      <Center mih="100vh" data-testid="auth-loading">
        <Loader />
      </Center>
    );
  }
  if (status === 'anonymous') {
    const from = `${location.pathname}${location.search}${location.hash}`;
    // The route is preserved so that signing in again lands where the user was (TD-E-17).
    return (
      <Navigate to="/login" replace state={expired ? { from, reason: 'expired' } : { from }} />
    );
  }
  return children ?? <Outlet />;
}
