import {
  IconActivity,
  IconAdjustments,
  IconBucket,
  IconDatabase,
  IconFiles,
  IconFlask,
  IconInfoCircle,
  IconKey,
  IconLayoutDashboard,
  IconServer,
  IconShare,
  IconShieldLock,
  IconUsers,
  IconUsersGroup,
  IconWorld,
  type Icon,
} from '@tabler/icons-react';

/** Sections of the console; each key has a label under nav.* in the locales. */
export const navKeys = [
  'overview',
  'buckets',
  'objects',
  'users',
  'groups',
  'policies',
  'simulator',
  'accessKeys',
  'shares',
  'distributions',
  'cache',
  'cluster',
  'monitoring',
  'configuration',
  'info',
] as const;

export type NavKey = (typeof navKeys)[number];

export interface NavItem {
  key: NavKey;
  /** Absolute route path. */
  path: string;
  icon: Icon;
  /**
   * Policy action needed to use the section; the menu hides the item once
   * the effective permissions are known and deny it (D-UX-02).
   */
  requires?: string;
}

export const navigation: readonly NavItem[] = [
  { key: 'overview', path: '/', icon: IconLayoutDashboard },
  { key: 'buckets', path: '/buckets', icon: IconBucket },
  { key: 'objects', path: '/objects', icon: IconFiles },
  { key: 'users', path: '/users', icon: IconUsers, requires: 'admin:ListUsers' },
  { key: 'groups', path: '/groups', icon: IconUsersGroup, requires: 'admin:ListGroups' },
  { key: 'policies', path: '/policies', icon: IconShieldLock, requires: 'admin:ListUserPolicies' },
  {
    key: 'simulator',
    path: '/policy-simulator',
    icon: IconFlask,
    requires: 'admin:SimulatePolicy',
  },
  { key: 'accessKeys', path: '/access-keys', icon: IconKey },
  { key: 'shares', path: '/shares', icon: IconShare },
  { key: 'distributions', path: '/distributions', icon: IconWorld },
  { key: 'cache', path: '/cache', icon: IconDatabase },
  { key: 'cluster', path: '/cluster', icon: IconServer },
  { key: 'monitoring', path: '/monitoring', icon: IconActivity },
  {
    key: 'configuration',
    path: '/configuration',
    icon: IconAdjustments,
    requires: 'admin:ConfigUpdate',
  },
  { key: 'info', path: '/info', icon: IconInfoCircle, requires: 'admin:ServerInfo' },
];

/** True when the item is the one matching the current pathname. */
export function isNavItemActive(item: NavItem, pathname: string): boolean {
  if (item.path === '/') {
    return pathname === '/';
  }
  return pathname === item.path || pathname.startsWith(`${item.path}/`);
}

/** The items a user may see: all of them until the permissions are known. */
export function visibleNavigation(
  items: readonly NavItem[],
  permissions: { ready: boolean; can: (action: string) => boolean },
): NavItem[] {
  return items.filter(
    (item) => item.requires === undefined || !permissions.ready || permissions.can(item.requires),
  );
}
