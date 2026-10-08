import i18n, { type i18n as I18nInstance } from 'i18next';
import { initReactI18next } from 'react-i18next';

import en from './locales/en.json';
import it from './locales/it.json';

export const supportedLanguages = ['en', 'it'] as const;
export type Language = (typeof supportedLanguages)[number];
export const defaultLanguage: Language = 'en';

/** localStorage key remembering the user's language choice. */
export const languageStorageKey = 'nexo.console.language';

export const resources = {
  en: { translation: en },
  it: { translation: it },
} as const;

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
 * Initialises i18next synchronously (resources are bundled) and binds it to
 * react-i18next. Calling it twice is a no-op, which keeps tests simple.
 */
export function initI18n(options: InitI18nOptions = {}): I18nInstance {
  if (!i18n.isInitialized) {
    void i18n.use(initReactI18next).init({
      resources,
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
  } else if (options.lng !== undefined && i18n.language !== options.lng) {
    void i18n.changeLanguage(options.lng);
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
