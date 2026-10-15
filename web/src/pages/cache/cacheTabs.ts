import { CacheActions, type PermissionCheck } from '@/auth/usePermissions';

/** The sections of the cache page, in their order. */
export const cacheTabs = [
  'overview',
  'policies',
  'invalidations',
  'warm',
  'inspector',
  'settings',
] as const;

export type CacheTab = (typeof cacheTabs)[number];

/**
 * The sections of the cache a user may open: the operations on the
 * cached content with admin:InvalidateCache, the rest with
 * admin:ManageCache (TD-C-06).
 */
export function visibleCacheTabs(perms: Pick<PermissionCheck, 'can'>): CacheTab[] {
  return cacheTabs.filter((tab) =>
    tab === 'invalidations' || tab === 'warm'
      ? perms.can(CacheActions.invalidate)
      : perms.can(CacheActions.manage),
  );
}
