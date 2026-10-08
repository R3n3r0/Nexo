import { Button, Center, Stack, Text, Title } from '@mantine/core';
import { useTranslation } from 'react-i18next';
import { Link, useLocation } from 'react-router';

export function NotFoundPage() {
  const { t } = useTranslation();
  const location = useLocation();
  return (
    <Center mih="60vh">
      <Stack align="center" gap="sm">
        <Text fz={64} fw={700} lh={1} c="dimmed">
          404
        </Text>
        <Title order={2}>{t('notFound.title')}</Title>
        <Text c="dimmed">{t('notFound.message', { path: location.pathname })}</Text>
        <Button component={Link} to="/" variant="default" mt="sm">
          {t('common.goHome')}
        </Button>
      </Stack>
    </Center>
  );
}
