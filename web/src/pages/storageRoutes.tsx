import { Center, Loader } from '@mantine/core';
import { lazy, Suspense, type ComponentType, type LazyExoticComponent } from 'react';
import { Navigate, type RouteObject } from 'react-router';

import { loadSectionTranslations } from '@/i18n';

/** Lazily loads a named page export, so that each section is its own chunk. */
function lazyPage<M>(load: () => Promise<M>, pick: (module: M) => ComponentType) {
  return lazy(async () => ({ default: pick(await load()) }));
}

const BucketsPage = lazyPage(
  () => import('./buckets/BucketsPage'),
  (m) => m.BucketsPage,
);
const BucketDetailPage = lazyPage(
  () => import('./buckets/BucketDetailPage'),
  (m) => m.BucketDetailPage,
);
const ObjectBrowser = lazyPage(
  () => import('./objects/ObjectBrowser'),
  (m) => m.ObjectBrowser,
);
const BucketSettingsTab = lazyPage(
  () => import('./buckets/BucketSettingsTab'),
  (m) => m.BucketSettingsTab,
);
const BucketLifecycleTab = lazyPage(
  () => import('./buckets/BucketLifecycleTab'),
  (m) => m.BucketLifecycleTab,
);
const BucketEventsTab = lazyPage(
  () => import('./buckets/BucketEventsTab'),
  (m) => m.BucketEventsTab,
);
// The page uses strings loaded on demand (i18n sections).
const BucketMediaTab = lazyPage(
  async () =>
    (await Promise.all([import('./buckets/BucketMediaTab'), loadSectionTranslations()]))[0],
  (m) => m.BucketMediaTab,
);
const BucketAccessTab = lazyPage(
  () => import('./buckets/BucketAccessTab'),
  (m) => m.BucketAccessTab,
);
// The page uses strings loaded on demand (i18n sections).
const BucketRulesTab = lazyPage(
  async () =>
    (await Promise.all([import('./buckets/BucketRulesTab'), loadSectionTranslations()]))[0],
  (m) => m.BucketRulesTab,
);
const ObjectsPage = lazyPage(
  () => import('./objects/ObjectsPage'),
  (m) => m.ObjectsPage,
);
// The TLS and tracing state use strings loaded on demand (i18n sections).
const InfoPage = lazyPage(
  async () => (await Promise.all([import('./InfoPage'), loadSectionTranslations()]))[0],
  (m) => m.InfoPage,
);

function page(Page: LazyExoticComponent<ComponentType>) {
  return (
    <Suspense
      fallback={
        <Center py="xl">
          <Loader />
        </Center>
      }
    >
      <Page />
    </Suspense>
  );
}

/**
 * Routes of the storage sections (buckets, object browser, server info),
 * children of the authenticated layout. The prefix of the object browser
 * is the splat of /buckets/:bucket/browse/*. The pages are loaded on demand
 * to keep them out of the initial bundle (docs/06 §6.5).
 */
export function storageRoutes(): RouteObject[] {
  return [
    { path: 'buckets', element: page(BucketsPage) },
    {
      path: 'buckets/:bucket',
      element: page(BucketDetailPage),
      children: [
        { index: true, element: <Navigate to="browse/" replace /> },
        { path: 'browse/*', element: page(ObjectBrowser) },
        { path: 'settings', element: page(BucketSettingsTab) },
        { path: 'lifecycle', element: page(BucketLifecycleTab) },
        { path: 'events', element: page(BucketEventsTab) },
        { path: 'media', element: page(BucketMediaTab) },
        { path: 'access', element: page(BucketAccessTab) },
        { path: 'rules', element: page(BucketRulesTab) },
      ],
    },
    { path: 'objects', element: page(ObjectsPage) },
    { path: 'info', element: page(InfoPage) },
  ];
}
