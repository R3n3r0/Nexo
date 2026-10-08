import 'i18next';

import type en from './locales/en.json';

// Typed translation keys: t('nav.overview') is checked against en.json.
declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: 'translation';
    resources: {
      translation: typeof en;
    };
    returnNull: false;
  }
}
