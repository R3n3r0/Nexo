import { Paper, Stack, Text, Title } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import type { NavKey } from '@/layout/navigation';

export interface PlaceholderPageProps {
  section: NavKey;
}

/** Page shown for sections whose UI has not been built yet. */
export function PlaceholderPage({ section }: PlaceholderPageProps) {
  const { t } = useTranslation();
  const title = t(`nav.${section}`);
  return (
    <Stack gap="md">
      <Title order={2}>{title}</Title>
      <Paper withBorder p="xl" radius="md">
        <Text c="dimmed">{t('common.comingSoon')}</Text>
      </Paper>
    </Stack>
  );
}
