import { Center, Loader } from '@mantine/core';
import { lazy, Suspense, type ComponentType, type LazyExoticComponent } from 'react';
import type { RouteObject } from 'react-router';

import { loadSectionTranslations } from '@/i18n';

/** Lazily loads a named page export, so that each section is its own chunk. */
function lazyPage<M>(load: () => Promise<M>, pick: (module: M) => ComponentType) {
  return lazy(async () => ({ default: pick(await load()) }));
}

const UsersPage = lazyPage(
  () => import('./iam/UsersPage'),
  (m) => m.UsersPage,
);
const GroupsPage = lazyPage(
  () => import('./iam/GroupsPage'),
  (m) => m.GroupsPage,
);
const PoliciesPage = lazyPage(
  () => import('./policies/PoliciesPage'),
  (m) => m.PoliciesPage,
);
const PolicyEditorPage = lazyPage(
  () => import('./policies/PolicyEditorPage'),
  (m) => m.PolicyEditorPage,
);
const PolicySimulatorPage = lazyPage(
  () => import('./policies/PolicySimulatorPage'),
  (m) => m.PolicySimulatorPage,
);
const AccessKeysPage = lazyPage(
  () => import('./accessKeys/AccessKeysPage'),
  (m) => m.AccessKeysPage,
);
const SharesPage = lazyPage(
  () => import('./shares/SharesPage'),
  (m) => m.SharesPage,
);
// The page uses strings loaded on demand (i18n sections).
const ConfigurationPage = lazyPage(
  async () =>
    (await Promise.all([import('./config/ConfigurationPage'), loadSectionTranslations()]))[0],
  (m) => m.ConfigurationPage,
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
 * Routes of the IAM, policy, sharing and configuration sections, children
 * of the authenticated layout. The pages are loaded on demand to keep them
 * out of the initial bundle (docs/06 §6.5).
 */
export function iamRoutes(): RouteObject[] {
  return [
    { path: 'users', element: page(UsersPage) },
    { path: 'groups', element: page(GroupsPage) },
    { path: 'policies', element: page(PoliciesPage) },
    { path: 'policies/new', element: page(PolicyEditorPage) },
    { path: 'policies/:name', element: page(PolicyEditorPage) },
    { path: 'policy-simulator', element: page(PolicySimulatorPage) },
    { path: 'access-keys', element: page(AccessKeysPage) },
    { path: 'shares', element: page(SharesPage) },
    { path: 'configuration', element: page(ConfigurationPage) },
  ];
}
