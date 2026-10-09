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
import { clusterRoutes } from '@/pages/clusterRoutes';
import { distributionRoutes } from '@/pages/distributionRoutes';
import { iamRoutes } from '@/pages/iamRoutes';
import { CachePage, MonitoringPage } from '@/pages/placeholders';
import { storageRoutes } from '@/pages/storageRoutes';
import { UploadProvider } from '@/upload/UploadProvider';

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
          // The upload queue outlives the navigation between the pages.
          element: (
            <UploadProvider>
              <AppShellLayout />
            </UploadProvider>
          ),
          children: [
            { index: true, element: <OverviewPage /> },
            ...storageRoutes(),
            ...iamRoutes(),
            ...distributionRoutes(),
            { path: 'cache', element: <CachePage /> },
            ...clusterRoutes(),
            { path: 'monitoring', element: <MonitoringPage /> },
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
