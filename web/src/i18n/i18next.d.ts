import 'i18next';

import type en from './locales/en.json';
import type enSections from './locales/en.sections.json';

// Typed translation keys: t('nav.overview') is checked against en.json and
// the strings of the sections loaded on demand (en.sections.json).
declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: 'translation';
    resources: {
      translation: typeof en & typeof enSections;
    };
    returnNull: false;
  }
}
