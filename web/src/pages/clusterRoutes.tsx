import { Center, Loader } from '@mantine/core';
import { lazy, Suspense, type ComponentType, type LazyExoticComponent } from 'react';
import type { RouteObject } from 'react-router';

import { loadSectionTranslations } from '@/i18n';

/** Lazily loads a named page export, so that each section is its own chunk. */
function lazyPage<M>(load: () => Promise<M>, pick: (module: M) => ComponentType) {
  return lazy(async () => ({ default: pick(await load()) }));
}

// The cluster pages use strings loaded on demand (i18n sections).
const ClusterPage = lazyPage(
  async () => (await Promise.all([import('./cluster/ClusterPage'), loadSectionTranslations()]))[0],
  (m) => m.ClusterPage,
);
const HealingPage = lazyPage(
  async () => (await Promise.all([import('./cluster/HealingPage'), loadSectionTranslations()]))[0],
  (m) => m.HealingPage,
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
 * Routes of the cluster sections (D-CLU-01 cluster, D-CLU-02 healing),
 * children of the authenticated layout, loaded on demand.
 */
export function clusterRoutes(): RouteObject[] {
  return [
    { path: 'cluster', element: page(ClusterPage) },
    { path: 'healing', element: page(HealingPage) },
  ];
}
