import { Center, Loader } from '@mantine/core';
import { lazy, Suspense, type ComponentType, type LazyExoticComponent } from 'react';
import type { RouteObject } from 'react-router';

import { loadSectionTranslations } from '@/i18n';

/**
 * Lazily loads a named page export with the strings of the sections
 * loaded on demand, so that the cache pages stay out of the initial bundle.
 */
function lazyPage<M>(load: () => Promise<M>, pick: (module: M) => ComponentType) {
  return lazy(async () => {
    const [module] = await Promise.all([load(), loadSectionTranslations()]);
    return { default: pick(module) };
  });
}

const CachePage = lazyPage(
  () => import('./cache/CachePage'),
  (m) => m.CachePage,
);
const CachePolicyEditorPage = lazyPage(
  () => import('./cache/CachePolicyEditorPage'),
  (m) => m.CachePolicyEditorPage,
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
 * Routes of the cache (D-CCH-01..07), children of the authenticated
 * layout, loaded on demand. A new policy lives under
 * "/cache/policies/-/new": "-" is not a valid policy name.
 */
export function cacheRoutes(): RouteObject[] {
  return [
    { path: 'cache', element: page(CachePage) },
    { path: 'cache/:tab', element: page(CachePage) },
    { path: 'cache/policies/-/new', element: page(CachePolicyEditorPage) },
    { path: 'cache/policies/:name', element: page(CachePolicyEditorPage) },
  ];
}
