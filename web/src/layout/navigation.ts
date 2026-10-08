import {
  IconActivity,
  IconAdjustments,
  IconBucket,
  IconDatabase,
  IconFiles,
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
}

export const navigation: readonly NavItem[] = [
  { key: 'overview', path: '/', icon: IconLayoutDashboard },
  { key: 'buckets', path: '/buckets', icon: IconBucket },
  { key: 'objects', path: '/objects', icon: IconFiles },
  { key: 'users', path: '/users', icon: IconUsers },
  { key: 'groups', path: '/groups', icon: IconUsersGroup },
  { key: 'policies', path: '/policies', icon: IconShieldLock },
  { key: 'accessKeys', path: '/access-keys', icon: IconKey },
  { key: 'shares', path: '/shares', icon: IconShare },
  { key: 'distributions', path: '/distributions', icon: IconWorld },
  { key: 'cache', path: '/cache', icon: IconDatabase },
  { key: 'cluster', path: '/cluster', icon: IconServer },
  { key: 'monitoring', path: '/monitoring', icon: IconActivity },
  { key: 'configuration', path: '/configuration', icon: IconAdjustments },
  { key: 'info', path: '/info', icon: IconInfoCircle },
];

/** True when the item is the one matching the current pathname. */
export function isNavItemActive(item: NavItem, pathname: string): boolean {
  if (item.path === '/') {
    return pathname === '/';
  }
  return pathname === item.path || pathname.startsWith(`${item.path}/`);
}
