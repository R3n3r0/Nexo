import { afterEach, describe, expect, it } from 'vitest';

import i18n, { changeLanguage, initI18n, resources } from './index';

describe('i18n loading', () => {
  afterEach(async () => {
    await changeLanguage('en');
  });

  it('bundles the default language only', () => {
    expect(Object.keys(resources)).toEqual(['en']);
    expect(i18n.hasResourceBundle('en', 'translation')).toBe(true);
  });

  it('loads Italian on first use and switches to it', async () => {
    await initI18n({ lng: 'en' });
    expect(i18n.t('common.close')).toBe('Close');
    await changeLanguage('it');
    expect(i18n.hasResourceBundle('it', 'translation')).toBe(true);
    expect(i18n.resolvedLanguage).toBe('it');
    expect(i18n.t('common.close')).not.toBe('Close');
    expect(document.documentElement.lang).toBe('it');
  });
});
