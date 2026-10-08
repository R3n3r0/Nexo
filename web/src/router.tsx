import {
  createBrowserRouter,
  createMemoryRouter,
  type RouteObject,
  type RouterProviderProps,
} from 'react-router';

import { AuthGuard } from '@/auth/AuthGuard';
import { RouteErrorPage } from '@/components/RouteErrorPage';
import { AppShellLayout } from '@/layout/AppShellLayout';
import { LoginPage, type LoginPageProps } from '@/pages/LoginPage';
import { NotFoundPage } from '@/pages/NotFoundPage';
import { OverviewPage } from '@/pages/OverviewPage';
import {
  AccessKeysPage,
  BucketsPage,
  CachePage,
  ClusterPage,
  ConfigurationPage,
  DistributionsPage,
  GroupsPage,
  InfoPage,
  MonitoringPage,
  ObjectsPage,
  PoliciesPage,
  SharesPage,
  UsersPage,
} from '@/pages/placeholders';

export type AppRouter = RouterProviderProps['router'];

export interface RouteOptions {
  login?: LoginPageProps;
}

/** Route table of the console (React Router data router). */
export function createRoutes(options: RouteOptions = {}): RouteObject[] {
  return [
    {
      path: '/login',
      element: <LoginPage {...options.login} />,
      errorElement: <RouteErrorPage />,
    },
    {
      path: '/',
      element: <AuthGuard />,
      errorElement: <RouteErrorPage />,
      children: [
        {
          element: <AppShellLayout />,
          children: [
            { index: true, element: <OverviewPage /> },
            { path: 'buckets', element: <BucketsPage /> },
            { path: 'objects', element: <ObjectsPage /> },
            { path: 'users', element: <UsersPage /> },
            { path: 'groups', element: <GroupsPage /> },
            { path: 'policies', element: <PoliciesPage /> },
            { path: 'access-keys', element: <AccessKeysPage /> },
            { path: 'shares', element: <SharesPage /> },
            { path: 'distributions', element: <DistributionsPage /> },
            { path: 'cache', element: <CachePage /> },
            { path: 'cluster', element: <ClusterPage /> },
            { path: 'monitoring', element: <MonitoringPage /> },
            { path: 'configuration', element: <ConfigurationPage /> },
            { path: 'info', element: <InfoPage /> },
            { path: '*', element: <NotFoundPage /> },
          ],
        },
      ],
    },
  ];
}

/** Router used by the application (browser history). */
export function createAppRouter(options: RouteOptions = {}): AppRouter {
  return createBrowserRouter(createRoutes(options));
}

/** Router used by tests (in-memory history). */
export function createTestRouter(initialEntries: string[], options: RouteOptions = {}): AppRouter {
  return createMemoryRouter(createRoutes(options), { initialEntries });
}
