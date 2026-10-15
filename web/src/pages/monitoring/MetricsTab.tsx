import {
  Button,
  Group,
  Paper,
  SegmentedControl,
  Select,
  SimpleGrid,
  Skeleton,
  Stack,
  Text,
  Title,
} from '@mantine/core';
import { IconRefresh } from '@tabler/icons-react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { lazy, Suspense, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router';

import { ErrorAlert } from '@/components/iam/common';
import {
  chartUnit,
  formatMetricValue,
  isChartedSeries,
  lastValue,
  metricCharts,
  metricRanges,
  refreshIntervals,
  refreshMs,
  type MetricChartSpec,
  type MetricRange,
  type RefreshInterval,
} from '@/lib/monitoring';
import { useServices, type MetricInfo } from '@/services';

import type { MetricChartLine } from './MetricChart';
import { useNodeNames } from './useNodeNames';

const MetricChart = lazy(() => import('./MetricChart'));

/** Query keys of the series of the charts. */
const metricKeys = {
  all: ['monitoring', 'series'] as const,
  list: ['monitoring', 'metrics'] as const,
  chart: (id: string, metrics: readonly string[], range: string, node: string) =>
    ['monitoring', 'series', 'chart', id, metrics, range, node] as const,
  compare: (metric: string, range: string) =>
    ['monitoring', 'series', 'compare', metric, range] as const,
  nodes: (range: string) => ['monitoring', 'series', 'nodes', range] as const,
};

function isRange(value: string | null): value is MetricRange {
  return value !== null && (metricRanges as readonly string[]).includes(value);
}

interface ChartViewProps {
  title: string;
  hint: string;
  unit: string;
  lines: MetricChartLine[];
  stepSeconds: number;
  pending: boolean;
  error: unknown;
}

/** One card of the page: the chart of some series, their latest values. */
function ChartView({ title, hint, unit, lines, stepSeconds, pending, error }: ChartViewProps) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const withData = lines.some((l) => l.points.some((p) => p.v !== null && p.v !== undefined));
  const latest = lines
    .map((l) => `${l.label} ${formatMetricValue(lastValue(l.points), unit, locale)}`)
    .join(' · ');
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap={4}>
        <Title order={4}>{title}</Title>
        <Text size="xs" c="dimmed">
          {hint}
        </Text>
        {pending ? (
          <Skeleton height={200} />
        ) : error !== null ? (
          <ErrorAlert error={error} />
        ) : !withData ? (
          <Text size="sm" c="dimmed" py="xl" ta="center">
            {t('monitoring.metrics.noData')}
          </Text>
        ) : (
          <>
            <Suspense fallback={<Skeleton height={200} />}>
              <MetricChart
                label={title}
                unit={unit}
                lines={lines}
                stepSeconds={stepSeconds}
                locale={locale}
              />
            </Suspense>
            <Text size="xs" c="dimmed">
              {t('monitoring.metrics.latest', { values: latest })}
            </Text>
          </>
        )}
      </Stack>
    </Paper>
  );
}

/** A chart of the page: its series of the cluster (or of one node), fetched together. */
function ChartCard({
  spec,
  known,
  range,
  node,
  refetch,
}: {
  spec: MetricChartSpec;
  known: readonly MetricInfo[];
  range: MetricRange;
  node: string;
  refetch: number | false;
}) {
  const { t } = useTranslation();
  const { monitoring } = useServices();
  const result = useQuery({
    queryKey: metricKeys.chart(spec.id, spec.metrics, range, node),
    queryFn: () =>
      Promise.all(
        spec.metrics.map((metric) =>
          monitoring.queryMetric({ metric, range, ...(node !== '' ? { nodes: [node] } : {}) }),
        ),
      ),
    refetchInterval: refetch,
  });
  const lines: MetricChartLine[] = spec.metrics.map((m, i) => ({
    key: m,
    label: isChartedSeries(m) ? t(`monitoring.metrics.series.${m}`) : m,
    points: result.data?.[i]?.cluster ?? [],
  }));
  return (
    <ChartView
      title={t(`monitoring.metrics.charts.${spec.id}`)}
      hint={t(`monitoring.metrics.charts.${spec.id}Hint`)}
      unit={chartUnit(spec, known)}
      lines={lines}
      stepSeconds={result.data?.[0]?.stepSeconds ?? 10}
      pending={result.isPending}
      error={result.error}
    />
  );
}

/**
 * The comparison of the nodes: one line per node for a series chosen
 * among those of the server (a cluster only).
 */
function CompareNodes({
  known,
  range,
  refetch,
}: {
  known: readonly MetricInfo[];
  range: MetricRange;
  refetch: number | false;
}) {
  const { t } = useTranslation();
  const { monitoring } = useServices();
  const [metric, setMetric] = useState('s3_requests');
  const info = known.find((m) => m.name === metric);
  const result = useQuery({
    queryKey: metricKeys.compare(metric, range),
    queryFn: () => monitoring.queryMetric({ metric, range }),
    refetchInterval: refetch,
  });
  const nodes = result.data?.nodes ?? [];
  return (
    <Stack gap="xs">
      <Select
        label={t('monitoring.metrics.compareMetric')}
        data={known.map((m) => ({ value: m.name, label: `${m.name} — ${m.description}` }))}
        value={metric}
        allowDeselect={false}
        searchable
        onChange={(v) => {
          if (v !== null) setMetric(v);
        }}
        maw={520}
      />
      <ChartView
        title={t('monitoring.metrics.compare')}
        hint={info?.description ?? t('monitoring.metrics.compareHint')}
        unit={info?.unit ?? ''}
        lines={nodes.map((n) => ({ key: n.node, label: n.node, points: n.points }))}
        stepSeconds={result.data?.stepSeconds ?? 10}
        pending={result.isPending}
        error={result.error}
      />
      {nodes
        .filter((n) => n.error !== undefined && n.error !== '')
        .map((n) => (
          <Text key={n.node} size="xs" c="dimmed">
            {t('monitoring.metrics.nodeError', { node: n.node, error: n.error ?? '' })}
          </Text>
        ))}
    </Stack>
  );
}

/**
 * D-MON-01: the charts of the deployment from the series of GET
 * /metrics/query (requests, errors, latency p50/p95/p99, bandwidth, disk
 * usage, drives, healing and scanner, internode calls, CPU, memory), for
 * the cluster or one node, over a time range, refreshed automatically.
 */
export function MetricsTab() {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { monitoring } = useServices();
  const queryClient = useQueryClient();
  const [params, setParams] = useSearchParams();
  const rangeParam = params.get('range');
  const range: MetricRange = isRange(rangeParam) ? rangeParam : '1h';
  const node = params.get('node') ?? '';
  const [refresh, setRefresh] = useState<RefreshInterval>('30s');
  const refetch = refreshMs(refresh);
  const knownNodes = useNodeNames();

  const list = useQuery({
    queryKey: metricKeys.list,
    queryFn: () => monitoring.metrics(),
    staleTime: Infinity,
  });
  // The nodes reporting series (also without admin:ServerInfo), and the
  // time of the last refresh.
  const probe = useQuery({
    queryKey: metricKeys.nodes(range),
    queryFn: () => monitoring.queryMetric({ metric: 'drives_online', range: '15m' }),
    refetchInterval: refetch,
    enabled: list.data?.some((m) => m.name === 'drives_online') ?? false,
  });
  const known = list.data ?? [];
  const names = new Set(known.map((m) => m.name));
  const charts: MetricChartSpec[] = metricCharts
    .map((spec) => ({ ...spec, metrics: spec.metrics.filter((m) => names.has(m)) }))
    .filter((spec) => spec.metrics.length > 0);
  const seenNodes = (probe.data?.nodes ?? []).map((n) => n.node);
  const nodes = [...new Set([...knownNodes, ...seenNodes, ...(node !== '' ? [node] : [])])];
  const multiNode = nodes.length > 1;

  const setParam = (key: string, value: string) => {
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (value === '') next.delete(key);
        else next.set(key, value);
        return next;
      },
      { replace: true },
    );
  };

  if (list.isError) {
    return (
      <ErrorAlert
        error={list.error}
        title={t('monitoring.metrics.loadError')}
        onRetry={() => void list.refetch()}
      />
    );
  }

  const scopeLabel = node === '' ? t('monitoring.metrics.cluster') : node;
  return (
    <Stack gap="md">
      <Group justify="space-between" align="flex-end" wrap="wrap" gap="sm">
        <Group align="flex-end" wrap="wrap" gap="sm">
          <Stack gap={4}>
            <Text size="sm" fw={500} id="metrics-range-label">
              {t('monitoring.metrics.range')}
            </Text>
            <SegmentedControl
              aria-labelledby="metrics-range-label"
              data={metricRanges.map((r) => ({
                value: r,
                label: t(`monitoring.metrics.ranges.${r}`),
              }))}
              value={range}
              onChange={(v) => {
                setParam('range', v === '1h' ? '' : v);
              }}
            />
          </Stack>
          <Select
            label={t('monitoring.metrics.scope')}
            data={[
              { value: '', label: t('monitoring.metrics.cluster') },
              ...nodes.map((n) => ({ value: n, label: n })),
            ]}
            value={node}
            allowDeselect={false}
            onChange={(v) => {
              setParam('node', v ?? '');
            }}
            w={240}
          />
          <Select
            label={t('monitoring.metrics.refresh')}
            data={refreshIntervals.map((r) => ({
              value: r,
              label: t(`monitoring.metrics.refreshOptions.${r}`),
            }))}
            value={refresh}
            allowDeselect={false}
            onChange={(v) => {
              setRefresh((v ?? '30s') as RefreshInterval);
            }}
            w={170}
          />
        </Group>
        <Group gap="sm">
          {probe.dataUpdatedAt > 0 ? (
            <Text size="xs" c="dimmed">
              {t('monitoring.metrics.updated', {
                time: new Date(probe.dataUpdatedAt).toLocaleTimeString(locale),
              })}
            </Text>
          ) : null}
          <Button
            variant="default"
            leftSection={<IconRefresh size={16} />}
            onClick={() => void queryClient.refetchQueries({ queryKey: metricKeys.all })}
          >
            {t('monitoring.metrics.refreshNow')}
          </Button>
        </Group>
      </Group>
      {list.isPending ? (
        <Skeleton height={240} />
      ) : (
        <SimpleGrid
          cols={{ base: 1, lg: 2 }}
          spacing="md"
          aria-label={t('monitoring.metrics.chartsLabel', { scope: scopeLabel })}
        >
          {charts
            .filter((spec) => !spec.clusterOnly || multiNode)
            .map((spec) => (
              <ChartCard
                key={spec.id}
                spec={spec}
                known={known}
                range={range}
                node={node}
                refetch={refetch}
              />
            ))}
        </SimpleGrid>
      )}
      {multiNode && node === '' && known.length > 0 ? (
        <CompareNodes known={known} range={range} refetch={refetch} />
      ) : null}
    </Stack>
  );
}
