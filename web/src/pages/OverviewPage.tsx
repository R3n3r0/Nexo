import { Alert, Button, Group, SimpleGrid, Stack, Text, Title } from '@mantine/core';
import { IconAlertCircle } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { StatTile } from '@/components/StatTile';
import { formatBytes, formatCompact, formatDuration } from '@/lib/format';
import { serverInfoQueryKey, summariseServerInfo } from '@/lib/serverInfo';
import { useServices } from '@/services';

export function OverviewPage() {
  const { t, i18n } = useTranslation();
  const { info } = useServices();
  const query = useQuery({
    queryKey: serverInfoQueryKey,
    queryFn: () => info.serverInfo(),
    refetchInterval: 30_000,
  });
  const locale = i18n.resolvedLanguage;
  const data = query.data;
  const loading = query.isPending;

  const counts = data ? summariseServerInfo(data) : null;
  const dash = '—';

  return (
    <Stack gap="lg">
      <Stack gap={2}>
        <Title order={2}>{t('overview.title')}</Title>
        <Text c="dimmed" size="sm">
          {t('overview.subtitle')}
        </Text>
      </Stack>

      {query.isError ? (
        <Alert color="red" variant="light" icon={<IconAlertCircle size={18} />} role="alert">
          <Group justify="space-between" wrap="nowrap">
            <Text>{t('overview.loadError')}</Text>
            <Button
              size="xs"
              variant="default"
              onClick={() => {
                void query.refetch();
              }}
            >
              {t('common.retry')}
            </Button>
          </Group>
        </Alert>
      ) : null}

      <SimpleGrid cols={{ base: 1, xs: 2, lg: 4 }} spacing="md">
        <StatTile
          label={t('overview.tiles.mode')}
          value={data ? t(`modes.${data.mode}`) : dash}
          hint={data?.region}
          loading={loading}
        />
        <StatTile
          label={t('overview.tiles.version')}
          value={data?.version ?? dash}
          hint={data?.commit}
          loading={loading}
        />
        <StatTile
          label={t('overview.tiles.uptime')}
          value={
            data
              ? formatDuration(data.uptimeSeconds, (unit, count) =>
                  t(`duration.${unit}`, { count }),
                )
              : dash
          }
          loading={loading}
        />
        <StatTile
          label={t('overview.tiles.buckets')}
          value={data ? formatCompact(data.buckets, locale) : dash}
          loading={loading}
        />
        <StatTile
          label={t('overview.tiles.objects')}
          value={data ? formatCompact(data.objects, locale) : dash}
          loading={loading}
        />
        <StatTile
          label={t('overview.tiles.usage')}
          value={data ? formatBytes(data.usage, locale) : dash}
          loading={loading}
        />
        <StatTile
          label={t('overview.tiles.nodes')}
          value={counts ? counts.nodesOnline : dash}
          hint={
            counts
              ? t('overview.ofTotal', { online: counts.nodesOnline, total: counts.nodes })
              : undefined
          }
          loading={loading}
        />
        <StatTile
          label={t('overview.tiles.drives')}
          value={counts ? counts.drivesOnline : dash}
          hint={
            counts
              ? t('overview.ofTotal', { online: counts.drivesOnline, total: counts.drives })
              : undefined
          }
          loading={loading}
        />
      </SimpleGrid>
    </Stack>
  );
}
