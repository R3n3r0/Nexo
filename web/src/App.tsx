import { MantineProvider } from '@mantine/core';
import { DatesProvider } from '@mantine/dates';
import { ModalsProvider } from '@mantine/modals';
import { Notifications } from '@mantine/notifications';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { RouterProvider } from 'react-router';

import { SessionProvider } from '@/auth/SessionProvider';
import { ErrorBoundary } from '@/components/ErrorBoundary';
import { createQueryClient } from '@/lib/queryClient';
import type { AppRouter } from '@/router';
import { ServicesProvider, type Services } from '@/services';
import { theme } from '@/theme';

export interface AppProps {
  services: Services;
  router: AppRouter;
  /** Query client to use; a default one is created otherwise. */
  queryClient?: QueryClient;
}

/**
 * Wires the providers: Mantine (theme, notifications, modals, dates),
 * TanStack Query, the services, the session and the router.
 */
export function App({ services, router, queryClient }: AppProps) {
  const [client] = useState(() => queryClient ?? createQueryClient());
  const { i18n } = useTranslation();

  return (
    <ErrorBoundary>
      <MantineProvider theme={theme} defaultColorScheme="auto">
        <Notifications position="top-right" />
        <ModalsProvider>
          <DatesProvider settings={{ locale: i18n.resolvedLanguage ?? 'en' }}>
            <QueryClientProvider client={client}>
              <ServicesProvider services={services}>
                <SessionProvider>
                  <RouterProvider router={router} />
                </SessionProvider>
              </ServicesProvider>
            </QueryClientProvider>
          </DatesProvider>
        </ModalsProvider>
      </MantineProvider>
    </ErrorBoundary>
  );
}
