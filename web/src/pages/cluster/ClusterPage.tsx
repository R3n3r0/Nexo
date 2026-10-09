import {
  Alert,
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
import { IconAlertTriangle, IconClockExclamation, IconRefresh } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { StatTile } from '@/components/StatTile';
import {
  clusterHealth,
  clusterRefreshMs,
  driveState,
  formatSkew,
  nodesOfPool,
  resourceColor,
  setStateColor,
  usedRatio,
} from '@/lib/cluster';
import { formatBytes, formatDuration, formatNumber } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { serverInfoQueryKey } from '@/lib/serverInfo';
import { useServices, type NodeInfo, type PoolInfo, type ServerInfo } from '@/services';

/** Severity colour of a usage meter. */
function meterColor(ratio: number): string {
  if (ratio >= 0.9) return 'red';
  if (ratio >= 0.8) return 'yellow';
  return 'blue';
}

/** Background of the rows of what does not answer. */
const offlineRow = { backgroundColor: 'var(--mantine-color-red-light)' };

function NodesTable({ info }: { info: ServerInfo }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('cluster.nodes.title')}>
      <Title order={4} mb="sm">
        {t('cluster.nodes.title')}
      </Title>
      <Table.ScrollContainer minWidth={760}>
        <Table aria-label={t('cluster.nodes.title')} verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('cluster.nodes.node')}</Table.Th>
              <Table.Th>{t('common.status')}</Table.Th>
              <Table.Th>{t('cluster.nodes.version')}</Table.Th>
              <Table.Th>{t('cluster.nodes.uptime')}</Table.Th>
              <Table.Th>{t('cluster.nodes.clock')}</Table.Th>
              <Table.Th>{t('cluster.nodes.drives')}</Table.Th>
              <Table.Th>{t('cluster.nodes.backgroundHealing')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {info.nodes.map((node) => {
              const online = node.disks.filter((d) => d.state !== 'offline').length;
              return (
                <Table.Tr
                  key={node.endpoint}
                  data-state={node.state}
                  style={node.state === 'offline' ? offlineRow : undefined}
                >
                  <Table.Td>
                    <Text fw={600} size="sm" style={{ overflowWrap: 'anywhere' }}>
                      {node.endpoint}
                    </Text>
                    {node.local ? (
                      <Text size="xs" c="dimmed">
                        {t('cluster.nodes.thisNode')}
                      </Text>
                    ) : null}
                  </Table.Td>
                  <Table.Td>
                    <Badge color={resourceColor[node.state]} variant="light">
                      {t(`overview.states.${node.state}`)}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{node.version === '' ? '—' : node.version}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">
                      {node.uptimeSeconds > 0
                        ? formatDuration(node.uptimeSeconds, (unit, count) =>
                            t(`duration.${unit}`, { count }),
                          )
                        : '—'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <ClockCell node={node} locale={locale} />
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">
                      {t('overview.ofTotal', { online, total: node.disks.length })}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    {node.healQueue ? (
                      <Text size="sm">
                        {t('cluster.nodes.queue', {
                          pending: formatNumber(node.healQueue.pending, locale),
                          healed: formatNumber(node.healQueue.healed, locale),
                        })}
                      </Text>
                    ) : (
                      <Text size="sm" c="dimmed">
                        —
                      </Text>
                    )}
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Paper>
  );
}

function ClockCell({ node, locale }: { node: NodeInfo; locale: string | undefined }) {
  const { t } = useTranslation();
  if (node.local) {
    return (
      <Text size="sm" c="dimmed">
        {t('cluster.nodes.reference')}
      </Text>
    );
  }
  if (!node.clockSkew) {
    return (
      <Text size="sm" c="dimmed">
        —
      </Text>
    );
  }
  const text = formatSkew(node.clockSkew.offsetMs, locale);
  return node.clockSkew.exceeded ? (
    <Badge color="yellow" variant="light" leftSection={<IconClockExclamation size={12} />}>
      {text}
    </Badge>
  ) : (
    <Text size="sm">{text}</Text>
  );
}

function PoolSection({ info, pool }: { info: ServerInfo; pool: PoolInfo }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const title = t('cluster.pool.title', { index: pool.index + 1 });
  const nodes = nodesOfPool(info, pool.index);
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="md">
        <Stack gap={2}>
          <Title order={4}>{title}</Title>
          <Text size="sm" c="dimmed">
            {pool.parity === 0
              ? t('cluster.pool.noErasure', { count: pool.drivesPerSet })
              : t('cluster.pool.summary', {
                  sets: pool.sets,
                  drives: pool.drivesPerSet,
                  parity: pool.parity,
                  read: pool.readQuorum,
                  write: pool.writeQuorum,
                })}
          </Text>
        </Stack>

        <Table.ScrollContainer minWidth={480}>
          <Table aria-label={t('cluster.sets.title', { index: pool.index + 1 })}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('cluster.sets.set')}</Table.Th>
                <Table.Th>{t('cluster.sets.state')}</Table.Th>
                <Table.Th>{t('cluster.sets.online')}</Table.Th>
                <Table.Th>{t('cluster.sets.offline')}</Table.Th>
                <Table.Th>{t('cluster.sets.healing')}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {pool.erasureSets.map((set) => (
                <Table.Tr key={set.index} data-state={set.state}>
                  <Table.Td>{t('cluster.sets.name', { index: set.index + 1 })}</Table.Td>
                  <Table.Td>
                    <Badge color={setStateColor[set.state]} variant="light">
                      {t(`cluster.setStates.${set.state}`)}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    {t('overview.ofTotal', { online: set.online, total: set.drives })}
                  </Table.Td>
                  <Table.Td>{set.offline}</Table.Td>
                  <Table.Td>{set.healing}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>

        <Table.ScrollContainer minWidth={640}>
          <Table
            aria-label={t('cluster.drives.title', { index: pool.index + 1 })}
            verticalSpacing="xs"
          >
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('cluster.drives.drive')}</Table.Th>
                <Table.Th>{t('cluster.drives.set')}</Table.Th>
                <Table.Th>{t('common.status')}</Table.Th>
                <Table.Th>{t('cluster.drives.usage')}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {nodes.map(({ node, disks }) => [
                <Table.Tr key={node.endpoint} data-state={node.state}>
                  <Table.Td colSpan={4}>
                    <Group gap="xs">
                      <Text fw={600} size="sm" style={{ overflowWrap: 'anywhere' }}>
                        {node.endpoint}
                      </Text>
                      {node.state === 'offline' ? (
                        <Badge color="red" variant="light" size="sm">
                          {t('overview.states.offline')}
                        </Badge>
                      ) : null}
                    </Group>
                  </Table.Td>
                </Table.Tr>,
                ...disks.map((disk) => {
                  const state = driveState(disk);
                  const ratio = usedRatio(disk);
                  const progress = disk.healingProgress;
                  return (
                    <Table.Tr
                      key={`${node.endpoint}:${disk.endpoint}`}
                      data-state={state}
                      style={state === 'offline' ? offlineRow : undefined}
                    >
                      <Table.Td pl="xl">
                        <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                          {disk.path === '' ? disk.endpoint : disk.path}
                        </Text>
                        {disk.error !== undefined && disk.error !== '' ? (
                          <Text size="xs" c="red">
                            {disk.error}
                          </Text>
                        ) : null}
                      </Table.Td>
                      <Table.Td>{disk.setIdx + 1}</Table.Td>
                      <Table.Td>
                        <Badge color={resourceColor[state]} variant="dot">
                          {t(`overview.states.${state}`)}
                        </Badge>
                        {progress ? (
                          <Text size="xs" c="dimmed" mt={2}>
                            {t('cluster.drives.healingProgress', {
                              objects: formatNumber(progress.objectsHealed, locale),
                              bytes: formatBytes(progress.bytesDone, locale),
                              failed: formatNumber(progress.objectsFailed, locale),
                            })}
                          </Text>
                        ) : null}
                      </Table.Td>
                      <Table.Td miw={180}>
                        {disk.total > 0 ? (
                          <>
                            <Progress
                              value={ratio * 100}
                              color={meterColor(ratio)}
                              size="sm"
                              aria-label={t('overview.nodes.driveUsage', { path: disk.path })}
                            />
                            <Text size="xs" c="dimmed" mt={2}>
                              {t('overview.nodes.usedOf', {
                                used: formatBytes(disk.used, locale),
                                total: formatBytes(disk.total, locale),
                              })}
                            </Text>
                          </>
                        ) : (
                          <Text size="sm" c="dimmed">
                            —
                          </Text>
                        )}
                      </Table.Td>
                    </Table.Tr>
                  );
                }),
              ])}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Stack>
    </Paper>
  );
}

/**
 * D-CLU-01: the deployment mode, the pools with their erasure sets and
 * quorums, the nodes (version, uptime, clock offset, background healing)
 * and their drives (state, usage, healing progress). What does not answer
 * is highlighted; the page refreshes itself every few seconds.
 */
export function ClusterPage() {
  const { t, i18n } = useTranslation();
  const { info } = useServices();
  const query = useQuery({
    queryKey: serverInfoQueryKey,
    queryFn: () => info.serverInfo(),
    refetchInterval: clusterRefreshMs,
  });
  const locale = i18n.resolvedLanguage;
  const data = query.data;
  const health = data ? clusterHealth(data) : undefined;
  const dash = '—';

  return (
    <Stack gap="lg">
      <Group justify="space-between" align="flex-start" wrap="wrap">
        <Stack gap={2}>
          <Title order={2}>{t('cluster.title')}</Title>
          <Text c="dimmed" size="sm">
            {t('cluster.subtitle')}
          </Text>
        </Stack>
        <Button
          variant="default"
          leftSection={<IconRefresh size={16} />}
          loading={query.isFetching}
          onClick={() => {
            void query.refetch();
          }}
        >
          {t('cluster.refresh')}
        </Button>
      </Group>

      {query.isError ? (
        <Alert color="red" variant="light" role="alert">
          {problemMessage(t, query.error)}
        </Alert>
      ) : null}

      {health && health.offlineNodes.length > 0 ? (
        <Alert
          color="red"
          variant="light"
          icon={<IconAlertTriangle size={18} />}
          title={t('cluster.alerts.nodesOffline', { count: health.offlineNodes.length })}
          role="alert"
        >
          {health.offlineNodes.join(', ')}
        </Alert>
      ) : null}
      {health && (health.state === 'readonly' || health.state === 'unavailable') ? (
        <Alert color="red" variant="light" icon={<IconAlertTriangle size={18} />} role="alert">
          {t(`cluster.alerts.${health.state}`)}
        </Alert>
      ) : null}
      {health && health.skewedNodes.length > 0 ? (
        <Alert
          color="yellow"
          variant="light"
          icon={<IconClockExclamation size={18} />}
          title={t('cluster.alerts.clockSkew')}
        >
          {t('cluster.alerts.clockSkewHint', { nodes: health.skewedNodes.join(', ') })}
        </Alert>
      ) : null}

      <SimpleGrid cols={{ base: 1, xs: 2, lg: 3 }} spacing="md">
        <StatTile
          label={t('cluster.tiles.mode')}
          value={data ? t(`modes.${data.mode}`) : dash}
          loading={query.isPending}
        />
        <StatTile
          label={t('cluster.tiles.health')}
          value={health ? t(`cluster.setStates.${health.state}`) : dash}
          hint={
            health
              ? t('cluster.tiles.setsOk', { ok: health.setsOk, total: health.sets })
              : undefined
          }
          loading={query.isPending}
        />
        <StatTile
          label={t('cluster.tiles.nodes')}
          value={
            health
              ? t('overview.ofTotal', { online: health.nodesOnline, total: health.nodes })
              : dash
          }
          loading={query.isPending}
        />
        <StatTile
          label={t('cluster.tiles.drives')}
          value={
            health
              ? t('overview.ofTotal', { online: health.drivesOnline, total: health.drives })
              : dash
          }
          hint={
            health ? t('cluster.tiles.drivesHealing', { count: health.drivesHealing }) : undefined
          }
          loading={query.isPending}
        />
        <StatTile
          label={t('cluster.tiles.backgroundHealing')}
          value={health ? formatNumber(health.healPending, locale) : dash}
          hint={
            health
              ? t('cluster.tiles.healed', {
                  count: health.healHealed,
                  formatted: formatNumber(health.healHealed, locale),
                })
              : undefined
          }
          loading={query.isPending}
        />
        <StatTile
          label={t('cluster.tiles.clockSkew')}
          value={
            health?.maxSkewMs === undefined
              ? dash
              : formatSkew(health.maxSkewMs, locale).replace('+', '')
          }
          hint={t('cluster.tiles.clockSkewHint')}
          loading={query.isPending}
        />
      </SimpleGrid>

      {query.isPending ? <Skeleton height={240} /> : null}
      {data ? <NodesTable info={data} /> : null}
      {data?.pools.map((pool) => (
        <PoolSection key={pool.index} info={data} pool={pool} />
      ))}
    </Stack>
  );
}
