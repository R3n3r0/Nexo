import { Center, Loader } from '@mantine/core';
import { lazy, Suspense, type ComponentType, type LazyExoticComponent } from 'react';
import type { RouteObject } from 'react-router';

import { loadSectionTranslations } from '@/i18n';

/**
 * Lazily loads a named page export, so that each section is its own chunk,
 * with the strings of the section (outside the initial bundle).
 */
function lazyPage<M>(load: () => Promise<M>, pick: (module: M) => ComponentType) {
  return lazy(async () => {
    const [module] = await Promise.all([load(), loadSectionTranslations()]);
    return { default: pick(module) };
  });
}

const DistributionsPage = lazyPage(
  () => import('./distributions/DistributionsPage'),
  (m) => m.DistributionsPage,
);
const DistributionWizard = lazyPage(
  () => import('./distributions/DistributionWizard'),
  (m) => m.DistributionWizard,
);
const DistributionDetailPage = lazyPage(
  () => import('./distributions/DistributionDetailPage'),
  (m) => m.DistributionDetailPage,
);
const KeyGroupsPage = lazyPage(
  () => import('./distributions/KeyGroupsPage'),
  (m) => m.KeyGroupsPage,
);
const CertificatesPage = lazyPage(
  () => import('./distributions/CertificatesPage'),
  (m) => m.CertificatesPage,
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
 * Routes of the distributions (D-DST-01..08), children of the
 * authenticated layout, loaded on demand. The pages that are not a
 * distribution live under "/distributions/-/": "-" is not a valid
 * distribution id, so no distribution can shadow them.
 */
export function distributionRoutes(): RouteObject[] {
  return [
    { path: 'distributions', element: page(DistributionsPage) },
    { path: 'distributions/-/new', element: page(DistributionWizard) },
    { path: 'distributions/-/key-groups', element: page(KeyGroupsPage) },
    { path: 'distributions/-/certificates', element: page(CertificatesPage) },
    { path: 'distributions/:id/:tab?', element: page(DistributionDetailPage) },
  ];
}
