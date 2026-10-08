import { MantineProvider } from '@mantine/core';
import { DatesProvider } from '@mantine/dates';
import { ModalsProvider } from '@mantine/modals';
import { Notifications } from '@mantine/notifications';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { RouterProvider } from 'react-router';

import { SessionProvider } from '@/auth/SessionProvider';
import { ErrorBoundary } from '@/components/ErrorBoundary';
import { createQueryClient } from '@/lib/queryClient';
import type { AppRouter } from '@/router';
import { ServicesProvider, type Services } from '@/services';
import { cssVariablesResolver, localizedTheme } from '@/theme';

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
  const { t, i18n } = useTranslation();
  // t changes with the language, and so do the labels of the theme.
  const theme = useMemo(
    () =>
      localizedTheme({
        close: t('common.close'),
        toggleSecret: t('common.toggleSecret'),
        dates: {
          previousMonth: t('dates.previousMonth'),
          nextMonth: t('dates.nextMonth'),
          previousYear: t('dates.previousYear'),
          nextYear: t('dates.nextYear'),
          previousDecade: t('dates.previousDecade'),
          nextDecade: t('dates.nextDecade'),
          monthLevel: t('dates.monthLevel'),
          yearLevel: t('dates.yearLevel'),
          time: t('dates.time'),
          submit: t('dates.submit'),
        },
      }),
    [t],
  );

  return (
    <ErrorBoundary>
      <MantineProvider
        theme={theme}
        cssVariablesResolver={cssVariablesResolver}
        defaultColorScheme="auto"
      >
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
