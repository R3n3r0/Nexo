import { SegmentedControl } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { changeLanguage, isLanguage, supportedLanguages } from '@/i18n';

/** Lets the user pick the UI language; the choice is remembered. */
export function LanguageSwitcher() {
  const { t, i18n } = useTranslation();
  const current = isLanguage(i18n.resolvedLanguage) ? i18n.resolvedLanguage : 'en';
  return (
    <SegmentedControl
      size="xs"
      aria-label={t('language.label')}
      value={current}
      onChange={(value) => {
        if (isLanguage(value)) {
          void changeLanguage(value);
        }
      }}
      data={supportedLanguages.map((lng) => ({ value: lng, label: lng.toUpperCase() }))}
    />
  );
}
