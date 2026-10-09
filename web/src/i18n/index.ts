import i18n, { type BackendModule, type i18n as I18nInstance } from 'i18next';
import { initReactI18next } from 'react-i18next';

import en from './locales/en.json';

export const supportedLanguages = ['en', 'it'] as const;
export type Language = (typeof supportedLanguages)[number];
export const defaultLanguage: Language = 'en';

/** localStorage key remembering the user's language choice. */
export const languageStorageKey = 'nexo.console.language';

/**
 * The bundled translations: the default language only. The others are
 * loaded on first use as separate chunks (lazyLocales), so that they do not
 * weigh on the initial bundle of the console.
 */
export const resources = {
  en: { translation: en },
} as const;

/**
 * Loads the translations of the languages that are not bundled: the core
 * strings and the strings of the sections loaded on demand, together.
 */
const lazyLocales: BackendModule = {
  type: 'backend',
  init: () => undefined,
  read(language, _namespace, callback) {
    if (language !== 'it') {
      callback(null, {});
      return;
    }
    Promise.all([import('./locales/it.json'), import('./locales/it.sections.json')]).then(
      ([core, sections]) => {
        callback(null, { ...core.default, ...sections.default });
      },
      (error: unknown) => {
        callback(error instanceof Error ? error : new Error(String(error)), null);
      },
    );
  },
};

let sectionsLoaded: Promise<void> | undefined;

/**
 * Loads the strings of the sections that are not in the initial bundle
 * (access rules, distributions, key groups, certificates: docs/06 §6.5
 * bundle budget) into the default language; the other languages load them
 * with their core strings. The route loaders of those sections await it
 * before rendering; later calls share the first load.
 */
export function loadSectionTranslations(): Promise<void> {
  sectionsLoaded ??= import('./locales/en.sections.json').then(
    (module) => {
      i18n.addResourceBundle(defaultLanguage, 'translation', module.default, true, false);
    },
    (error: unknown) => {
      sectionsLoaded = undefined;
      throw error;
    },
  );
  return sectionsLoaded;
}

export function isLanguage(value: unknown): value is Language {
  return typeof value === 'string' && (supportedLanguages as readonly string[]).includes(value);
}

/**
 * Picks the UI language: the stored preference first, then the first
 * browser language we support, then the default.
 */
export function detectLanguage(
  stored: string | null | undefined,
  browserLanguages: readonly string[],
): Language {
  if (isLanguage(stored)) {
    return stored;
  }
  for (const tag of browserLanguages) {
    const base = tag.toLowerCase().split('-')[0];
    if (isLanguage(base)) {
      return base;
    }
  }
  return defaultLanguage;
}

export interface InitI18nOptions {
  lng?: Language;
}

/**
 * Initialises i18next and binds it to react-i18next. The default language
 * is bundled and ready at once (the tests do not wait); the promise
 * resolves when the requested language is loaded too. Calling it twice
 * only changes the language, which keeps tests simple.
 */
export async function initI18n(options: InitI18nOptions = {}): Promise<I18nInstance> {
  if (!i18n.isInitialized) {
    const ready = i18n
      .use(lazyLocales)
      .use(initReactI18next)
      .init({
        resources,
        partialBundledLanguages: true,
        lng: options.lng ?? defaultLanguage,
        fallbackLng: defaultLanguage,
        supportedLngs: [...supportedLanguages],
        initImmediate: false,
        interpolation: { escapeValue: false },
        returnNull: false,
      });
    i18n.on('languageChanged', (lng) => {
      if (typeof document !== 'undefined') {
        document.documentElement.lang = lng;
      }
    });
    await ready;
  } else if (options.lng !== undefined && i18n.language !== options.lng) {
    await i18n.changeLanguage(options.lng);
  }
  return i18n;
}

/** Changes the language and remembers the choice. */
export async function changeLanguage(lng: Language): Promise<void> {
  try {
    localStorage.setItem(languageStorageKey, lng);
  } catch {
    // Storage may be unavailable (private mode); the change still applies.
  }
  await i18n.changeLanguage(lng);
}

export default i18n;
