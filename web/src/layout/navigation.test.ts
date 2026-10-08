import { describe, expect, it } from 'vitest';

import en from '@/i18n/locales/en.json';

import { isNavItemActive, navigation, navKeys } from './navigation';

describe('navigation', () => {
  it('lists every section exactly once with a translation', () => {
    expect(navigation.map((item) => item.key)).toEqual([...navKeys]);
    for (const key of navKeys) {
      expect(en.nav[key], key).toBeTypeOf('string');
    }
  });

  it('matches the active item by path prefix, except for the overview', () => {
    const overview = navigation[0];
    const buckets = navigation.find((item) => item.key === 'buckets');
    if (!overview || !buckets) throw new Error('fixture');
    expect(isNavItemActive(overview, '/')).toBe(true);
    expect(isNavItemActive(overview, '/buckets')).toBe(false);
    expect(isNavItemActive(buckets, '/buckets')).toBe(true);
    expect(isNavItemActive(buckets, '/buckets/photos')).toBe(true);
    expect(isNavItemActive(buckets, '/bucketsx')).toBe(false);
  });
});
