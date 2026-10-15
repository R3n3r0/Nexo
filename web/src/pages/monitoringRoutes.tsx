import { Center, Loader } from '@mantine/core';
import { lazy, Suspense } from 'react';
import type { RouteObject } from 'react-router';

import { loadSectionTranslations } from '@/i18n';

/**
 * The monitoring page with the strings of the sections loaded on demand:
 * the page, its charts and its streams stay out of the initial bundle.
 */
const MonitoringPage = lazy(async () => {
  const [module] = await Promise.all([
    import('./monitoring/MonitoringPage'),
    loadSectionTranslations(),
  ]);
  return { default: module.MonitoringPage };
});

function page() {
  return (
    <Suspense
      fallback={
        <Center py="xl">
          <Loader />
        </Center>
      }
    >
      <MonitoringPage />
    </Suspense>
  );
}

/**
 * Routes of the monitoring (D-MON-01..03): /monitoring opens the first
 * section the user may read, /monitoring/{metrics,trace,logs,audit} one
 * of them.
 */
export function monitoringRoutes(): RouteObject[] {
  return [
    { path: 'monitoring', element: page() },
    { path: 'monitoring/:tab', element: page() },
  ];
}
