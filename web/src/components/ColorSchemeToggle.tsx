import { ActionIcon, useComputedColorScheme, useMantineColorScheme } from '@mantine/core';
import { IconMoon, IconSun } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';

/** Switches between the light and dark colour schemes. */
export function ColorSchemeToggle() {
  const { t } = useTranslation();
  const { setColorScheme } = useMantineColorScheme();
  const computed = useComputedColorScheme('light', { getInitialValueInEffect: true });
  const next = computed === 'dark' ? 'light' : 'dark';
  return (
    <ActionIcon
      variant="default"
      size="lg"
      aria-label={t('theme.toggle')}
      title={t(`theme.${next}`)}
      onClick={() => {
        setColorScheme(next);
      }}
    >
      {computed === 'dark' ? <IconSun size={18} /> : <IconMoon size={18} />}
    </ActionIcon>
  );
}
