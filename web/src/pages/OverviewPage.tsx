import {
  Alert,
  Anchor,
  Badge,
  Button,
  Group,
  Paper,
  Progress,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  Text,
  Title,
} from '@mantine/core';
import { IconAlertCircle, IconInfoCircle } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { lazy, Suspense } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { StatTile } from '@/components/StatTile';
import { formatBytes, formatCompact, formatDuration } from '@/lib/format';
import { serverInfoQueryKey, summariseServerInfo } from '@/lib/serverInfo';
import { storageKeys } from '@/lib/storageKeys';
import {
  isApiError,
  useServices,
  type DataUsage,
  type ResourceState,
  type ServerInfo,
} from '@/services';

// Recharts is large: the chart is loaded only when the overview shows it.
const BucketUsageChart = lazy(() => import('./overview/BucketUsageChart'));

const stateColor: Record<ResourceState, string> = {
  online: 'green',
  offline: 'red',
  healing: 'yellow',
  unknown: 'gray',
};

/** Raw capacity of the drives: total, used and free bytes. */
// eslint-disable-next-line react-refresh/only-export-components -- pure helper tested with the page
export function capacityOf(info: ServerInfo): { total: number; used: number; free: number } {
  let total = 0;
  let used = 0;
  let free = 0;
  for (const node of info.nodes) {
    for (const disk of node.disks) {
      total += disk.total;
      used += disk.used;
      free += disk.free;
    }
  }
  return { total, used, free };
}

/** The ten largest buckets of a usage report, largest first. */
// eslint-disable-next-line react-refresh/only-export-components -- pure helper tested with the page
export function topBuckets(usage: DataUsage, limit = 10): { bucket: string; size: number }[] {
  return Object.entries(usage.buckets)
    .map(([bucket, u]) => ({ bucket, size: u.size }))
    .sort((a, b) => b.size - a.size || a.bucket.localeCompare(b.bucket))
    .slice(0, limit);
}

/** Meter severity: accent below 80%, warning up to 90%, danger above. */
function meterColor(ratio: number): string {
  if (ratio >= 0.9) return 'red';
  if (ratio >= 0.8) return 'yellow';
  return 'blue';
}

function CapacityMeter({ info }: { info: ServerInfo }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { total, used, free } = capacityOf(info);
  const ratio = total > 0 ? used / total : 0;
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('overview.capacity.title')}
    >
      <Stack gap="xs">
        <Group justify="space-between" wrap="wrap">
          <Title order={4}>{t('overview.capacity.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('overview.capacity.percentUsed', { percent: Math.round(ratio * 100) })}
          </Text>
        </Group>
        <Progress
          value={ratio * 100}
          color={meterColor(ratio)}
          size="lg"
          radius="sm"
          aria-label={t('overview.capacity.meter')}
        />
        <Group gap="xl" wrap="wrap">
          <Stack gap={0}>
            <Text size="xs" c="dimmed">
              {t('overview.capacity.used')}
            </Text>
            <Text fw={600}>{formatBytes(used, locale)}</Text>
          </Stack>
          <Stack gap={0}>
            <Text size="xs" c="dimmed">
              {t('overview.capacity.free')}
            </Text>
            <Text fw={600}>{formatBytes(free, locale)}</Text>
          </Stack>
          <Stack gap={0}>
            <Text size="xs" c="dimmed">
              {t('overview.capacity.total')}
            </Text>
            <Text fw={600}>{formatBytes(total, locale)}</Text>
          </Stack>
        </Group>
      </Stack>
    </Paper>
  );
}

function NodesTable({ info }: { info: ServerInfo }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('overview.nodes.title')}>
      <Title order={4} mb="sm">
        {t('overview.nodes.title')}
      </Title>
      <Table.ScrollContainer minWidth={520}>
        <Table aria-label={t('overview.nodes.title')} verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('overview.nodes.drive')}</Table.Th>
              <Table.Th>{t('common.status')}</Table.Th>
              <Table.Th>{t('overview.nodes.usage')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {info.nodes.map((node) => [
              <Table.Tr key={node.endpoint}>
                <Table.Td>
                  <Text fw={600} size="sm" style={{ overflowWrap: 'anywhere' }}>
                    {node.endpoint}
                  </Text>
                  <Text size="xs" c="dimmed">
                    {node.version}
                  </Text>
                </Table.Td>
                <Table.Td>
                  <Badge color={stateColor[node.state]} variant="light">
                    {t(`overview.states.${node.state}`)}
                  </Badge>
                </Table.Td>
                <Table.Td />
              </Table.Tr>,
              ...node.disks.map((disk) => (
                <Table.Tr key={`${node.endpoint}:${disk.path}`}>
                  <Table.Td pl="xl">
                    <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                      {disk.path}
                    </Text>
                    {disk.error !== undefined && disk.error !== '' ? (
                      <Text size="xs" c="red">
                        {disk.error}
                      </Text>
                    ) : null}
                  </Table.Td>
                  <Table.Td>
                    <Badge color={stateColor[disk.healing ? 'healing' : disk.state]} variant="dot">
                      {t(`overview.states.${disk.healing ? 'healing' : disk.state}`)}
                    </Badge>
                  </Table.Td>
                  <Table.Td miw={180}>
                    <Progress
                      value={disk.total > 0 ? (disk.used / disk.total) * 100 : 0}
                      color={meterColor(disk.total > 0 ? disk.used / disk.total : 0)}
                      size="sm"
                      aria-label={t('overview.nodes.driveUsage', { path: disk.path })}
                    />
                    <Text size="xs" c="dimmed" mt={2}>
                      {t('overview.nodes.usedOf', {
                        used: formatBytes(disk.used, locale),
                        total: formatBytes(disk.total, locale),
                      })}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              )),
            ])}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Paper>
  );
}

function BucketUsage() {
  const { t } = useTranslation();
  const { info } = useServices();
  const usage = useQuery({
    queryKey: storageKeys.usage,
    queryFn: () => info.usage(),
    refetchInterval: 60_000,
  });
  const data = usage.data ? topBuckets(usage.data) : [];
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('overview.charts.perBucket')}
    >
      <Stack gap="sm">
        <Stack gap={2}>
          <Title order={4}>{t('overview.charts.perBucket')}</Title>
          <Text size="sm" c="dimmed">
            {t('overview.charts.perBucketHint')}
          </Text>
        </Stack>
        {usage.isPending ? (
          <Skeleton height={160} />
        ) : usage.isError ? (
          <Text size="sm" c="dimmed">
            {t('overview.charts.unavailable')}
          </Text>
        ) : data.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('overview.charts.noBuckets')}
          </Text>
        ) : (
          <Suspense fallback={<Skeleton height={160} />}>
            <BucketUsageChart data={data} />
          </Suspense>
        )}
      </Stack>
    </Paper>
  );
}

/** D-DASH-01: mode, capacity, buckets/objects, nodes and drives, usage per bucket. */
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

      {query.isError && isApiError(query.error) && query.error.isForbidden ? (
        <Paper withBorder radius="md" p="md" role="status">
          <Group gap="sm" wrap="nowrap" align="flex-start">
            <IconInfoCircle size={18} aria-hidden />
            <Stack gap={4} align="flex-start">
              <Text>{t('overview.forbidden')}</Text>
              <Anchor component={Link} to="/buckets" size="sm">
                {t('overview.goToBuckets')}
              </Anchor>
            </Stack>
          </Group>
        </Paper>
      ) : query.isError ? (
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

      {data ? (
        <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
          <CapacityMeter info={data} />
          <BucketUsage />
        </SimpleGrid>
      ) : null}
      {data && data.nodes.length > 0 ? <NodesTable info={data} /> : null}
    </Stack>
  );
}
