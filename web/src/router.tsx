import { Center, Loader } from '@mantine/core';
import { lazy, Suspense } from 'react';
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
import { clusterRoutes } from '@/pages/clusterRoutes';
import { distributionRoutes } from '@/pages/distributionRoutes';
import { iamRoutes } from '@/pages/iamRoutes';
import { cacheRoutes } from '@/pages/cacheRoutes';
import { monitoringRoutes } from '@/pages/monitoringRoutes';
import { storageRoutes } from '@/pages/storageRoutes';
import { UploadProvider } from '@/upload/UploadProvider';

export type AppRouter = RouterProviderProps['router'];

// The overview is a chunk of its own too: the initial bundle holds the
// shell, the login and the services only (docs/06 §6.5).
const OverviewPage = lazy(async () => ({
  default: (await import('@/pages/OverviewPage')).OverviewPage,
}));

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
            {
              index: true,
              element: (
                <Suspense
                  fallback={
                    <Center py="xl">
                      <Loader />
                    </Center>
                  }
                >
                  <OverviewPage />
                </Suspense>
              ),
            },
            ...storageRoutes(),
            ...iamRoutes(),
            ...distributionRoutes(),
            ...cacheRoutes(),
            ...clusterRoutes(),
            ...monitoringRoutes(),
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
