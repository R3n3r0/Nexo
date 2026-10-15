import {
  Anchor,
  Badge,
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
import { lazy, Suspense, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { StatTile } from '@/components/StatTile';

import { cacheableRequests, formatRatio, hostUsage, totalRequests } from '@/lib/cache';
import { formatBytes, formatCompact, formatNumber } from '@/lib/format';
import type { CacheHostStats, CacheNodeStats, CacheObjectStat, CacheStats } from '@/services';

import { ScrollTable } from './ScrollTable';

const CacheCharts = lazy(() => import('./CacheCharts'));

/** The tiles of the statistics: requests, hit ratio, bandwidth saved, sizes. */
export function StatsTiles({ stats, nodes }: { stats: CacheStats; nodes: boolean }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const requests = totalRequests(stats);
  const memoryLimit = stats.nodes.reduce((s, n) => s + n.memoryLimitBytes, 0);
  const diskLimit = stats.nodes.reduce((s, n) => s + n.diskLimitBytes, 0);
  return (
    <SimpleGrid cols={{ base: 1, xs: 2, md: 3, xl: 6 }} spacing="md">
      <StatTile
        label={t('cache.stats.requests')}
        value={formatCompact(requests, locale)}
        hint={t('cache.stats.hits', {
          hits: formatNumber(
            (stats.requests.HIT ?? 0) +
              (stats.requests.STALE ?? 0) +
              (stats.requests.REVALIDATED ?? 0),
            locale,
          ),
        })}
      />
      <StatTile
        label={t('cache.stats.hitRatio')}
        value={formatRatio(stats.hitRatio, cacheableRequests(stats.requests), locale)}
        hint={t('cache.stats.hitRatioHint')}
      />
      <StatTile
        label={t('cache.stats.saved')}
        value={formatBytes(stats.bytesCache, locale)}
        hint={t('cache.stats.fromOrigin', { bytes: formatBytes(stats.bytesOrigin, locale) })}
      />
      <StatTile
        label={t('cache.stats.memory')}
        value={formatBytes(stats.memoryBytes, locale)}
        hint={
          nodes && memoryLimit > 0
            ? t('cache.stats.ofLimit', { limit: formatBytes(memoryLimit, locale) })
            : t('cache.stats.tierMemory')
        }
      />
      <StatTile
        label={t('cache.stats.disk')}
        value={formatBytes(stats.diskBytes, locale)}
        hint={
          nodes && diskLimit > 0
            ? t('cache.stats.ofLimit', { limit: formatBytes(diskLimit, locale) })
            : nodes && stats.nodes.length > 0
              ? t('cache.stats.noDisk')
              : t('cache.stats.tierDisk')
        }
      />
      <StatTile label={t('cache.stats.entries')} value={formatCompact(stats.entries, locale)} />
    </SimpleGrid>
  );
}

function Usage({ used, limit }: { used: number; limit: number }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  if (limit <= 0) {
    return (
      <Text size="xs" c="dimmed">
        {used > 0 ? formatBytes(used, locale) : t('cache.stats.off')}
      </Text>
    );
  }
  const pct = Math.min(100, (used / limit) * 100);
  const label = t('cache.stats.usedOf', {
    used: formatBytes(used, locale),
    limit: formatBytes(limit, locale),
  });
  return (
    <Stack gap={2} miw={120}>
      <Text size="xs">{label}</Text>
      <Progress value={pct} size="sm" aria-label={label} />
    </Stack>
  );
}

/** The cache of every node: tiers, entries, hit ratio, synchronisation. */
export function NodesTable({ nodes }: { nodes: readonly CacheNodeStats[] }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  return (
    <ScrollTable minWidth={760} label={t('cache.stats.nodesTable')}>
      <Table aria-label={t('cache.stats.nodesTable')} verticalSpacing={6}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t('cache.nodes.node')}</Table.Th>
            <Table.Th>{t('cache.nodes.state')}</Table.Th>
            <Table.Th>{t('cache.stats.memory')}</Table.Th>
            <Table.Th>{t('cache.stats.disk')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.entries')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.hitRatio')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.evictions')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.webhookQueue')}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {nodes.map((n) => (
            <Table.Tr key={n.node}>
              <Table.Td>
                <Text size="sm" ff="monospace">
                  {n.node}
                </Text>
                <Text size="xs" c="dimmed">
                  {t(`cache.modes.${n.mode}`)}
                </Text>
              </Table.Td>
              <Table.Td>
                <Group gap={4}>
                  <Badge size="sm" variant="light" color={n.online ? 'teal' : 'red'}>
                    {n.online ? t('cache.nodes.online') : t('cache.nodes.offline')}
                  </Badge>
                  {n.online && !n.synced ? (
                    <Badge size="sm" variant="light" color="yellow">
                      {t('cache.nodes.syncing')}
                    </Badge>
                  ) : null}
                </Group>
                {n.error !== undefined && n.error !== '' ? (
                  <Text size="xs" c="red">
                    {n.error}
                  </Text>
                ) : null}
              </Table.Td>
              <Table.Td>
                <Usage used={n.memoryBytes} limit={n.memoryLimitBytes} />
              </Table.Td>
              <Table.Td>
                <Usage used={n.diskBytes} limit={n.diskLimitBytes} />
              </Table.Td>
              <Table.Td ta="right">{formatNumber(n.entries, locale)}</Table.Td>
              <Table.Td ta="right">
                {formatRatio(n.hitRatio, cacheableRequests(n.requests), locale)}
              </Table.Td>
              <Table.Td ta="right">{formatNumber(n.evictions, locale)}</Table.Td>
              <Table.Td ta="right">{formatNumber(n.webhookQueue, locale)}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </ScrollTable>
  );
}

/** The domains, busiest first: requests, hit ratio, bytes, size in cache. */
export function DomainsStatsTable({
  hosts,
  label,
  linkDistributions = false,
}: {
  hosts: readonly CacheHostStats[];
  label: string;
  linkDistributions?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const sorted = [...hosts].sort((a, b) => totalRequests(b) - totalRequests(a));
  if (sorted.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {t('cache.stats.noDomains')}
      </Text>
    );
  }
  return (
    <ScrollTable minWidth={720} label={label}>
      <Table aria-label={label} verticalSpacing={4}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t('cache.stats.domain')}</Table.Th>
            <Table.Th>{t('cache.distribution')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.requests')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.hitRatio')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.fromCacheColumn')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.fromOriginColumn')}</Table.Th>
            <Table.Th ta="right">{t('cache.stats.inCache')}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {sorted.map((h) => {
            const requests = totalRequests(h);
            return (
              <Table.Tr key={`${h.distribution}|${h.host}`} data-host={h.host}>
                <Table.Td>
                  <Text size="sm" ff="monospace">
                    {h.host}
                  </Text>
                </Table.Td>
                <Table.Td>
                  {linkDistributions ? (
                    <Anchor
                      component={Link}
                      to={`/distributions/${encodeURIComponent(h.distribution)}/cache`}
                      size="sm"
                    >
                      {h.distribution}
                    </Anchor>
                  ) : (
                    <Text size="sm">{h.distribution}</Text>
                  )}
                </Table.Td>
                <Table.Td ta="right">{formatNumber(requests, locale)}</Table.Td>
                <Table.Td ta="right" data-column="hit-ratio">
                  {formatRatio(h.hitRatio, cacheableRequests(h.requests), locale)}
                </Table.Td>
                <Table.Td ta="right">{formatBytes(h.bytesCache, locale)}</Table.Td>
                <Table.Td ta="right">{formatBytes(h.bytesOrigin, locale)}</Table.Td>
                <Table.Td ta="right">{formatBytes(hostUsage(h), locale)}</Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
    </ScrollTable>
  );
}

/** The most requested objects, or the ones with the most misses. */
export function TopObjectsTable({
  objects,
  label,
  misses,
}: {
  objects: readonly CacheObjectStat[];
  label: string;
  misses: boolean;
}) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  if (objects.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {t('cache.stats.noObjects')}
      </Text>
    );
  }
  return (
    <ScrollTable minWidth={480} label={label}>
      <Table aria-label={label} verticalSpacing={2}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t('cache.stats.object')}</Table.Th>
            <Table.Th ta="right">
              {misses ? t('cache.stats.misses') : t('cache.stats.requests')}
            </Table.Th>
            <Table.Th ta="right">
              {misses ? t('cache.stats.requests') : t('cache.stats.misses')}
            </Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {objects.map((o) => (
            <Table.Tr key={`${o.distribution}|${o.host}|${o.path}`}>
              <Table.Td>
                <Text size="xs" ff="monospace" style={{ wordBreak: 'break-all' }}>
                  {o.host}
                  {o.path}
                </Text>
              </Table.Td>
              <Table.Td ta="right">{formatNumber(misses ? o.misses : o.requests, locale)}</Table.Td>
              <Table.Td ta="right">{formatNumber(misses ? o.requests : o.misses, locale)}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </ScrollTable>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Title order={4}>{title}</Title>
        {children}
      </Stack>
    </Paper>
  );
}

export interface StatsViewProps {
  stats: CacheStats;
  /** The cluster view: the nodes and the links to the distributions. */
  global: boolean;
}

/**
 * D-CCH-01 and D-CCH-08: the statistics of the cache (of the cluster or
 * of a distribution): headline figures, the nodes, the domains busiest
 * first with their charts, the top objects and the top misses.
 */
export function StatsView({ stats, global }: StatsViewProps) {
  const { t } = useTranslation();
  return (
    <Stack gap="md">
      <StatsTiles stats={stats} nodes={global} />
      {global ? (
        <Section title={t('cache.stats.nodesTitle')}>
          <NodesTable nodes={stats.nodes} />
        </Section>
      ) : null}
      <Section title={global ? t('cache.stats.topDomains') : t('cache.stats.domains')}>
        <DomainsStatsTable
          hosts={stats.hosts}
          linkDistributions={global}
          label={global ? t('cache.stats.topDomains') : t('cache.stats.domains')}
        />
        {stats.hosts.length > 0 ? (
          <Suspense fallback={<Skeleton height={200} />}>
            <CacheCharts
              hosts={[...stats.hosts]
                .sort((a, b) => totalRequests(b) - totalRequests(a))
                .slice(0, 10)}
            />
          </Suspense>
        ) : null}
      </Section>
      <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
        <Section title={t('cache.stats.topObjects')}>
          <TopObjectsTable
            objects={stats.topObjects}
            label={t('cache.stats.topObjects')}
            misses={false}
          />
        </Section>
        <Section title={t('cache.stats.topMisses')}>
          <TopObjectsTable objects={stats.topMisses} label={t('cache.stats.topMisses')} misses />
        </Section>
      </SimpleGrid>
    </Stack>
  );
}
