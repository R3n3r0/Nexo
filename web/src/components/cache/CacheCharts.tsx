/**
 * D-CCH-08: the charts of the cache statistics per domain. Charts are
 * large (Recharts): this module is only reached through a dynamic import,
 * so it stays out of the initial bundle and of the pages that do not
 * draw them.
 */
import '@mantine/charts/styles.css';

import { BarChart } from '@mantine/charts';
import { SimpleGrid, Stack, Text } from '@mantine/core';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { cacheableRequests, cacheResults, type CacheResult } from '@/lib/cache';
import { formatBytes, formatNumber } from '@/lib/format';
import type { CacheHostStats } from '@/services';

/**
 * The categorical colours of the X-Cache results, in a fixed order (a
 * colour follows its result, whatever the domains shown), stepped for the
 * light and the dark surfaces; validated for colour-vision deficiencies.
 */
const resultColors: Readonly<Record<CacheResult, string>> = {
  HIT: 'light-dark(#2a78d6, #3987e5)',
  MISS: 'light-dark(#eb6834, #d95926)',
  STALE: 'light-dark(#1baf7a, #199e70)',
  BYPASS: 'light-dark(#eda100, #c98500)',
  EXPIRED: 'light-dark(#e87ba4, #d55181)',
  REVALIDATED: 'light-dark(#008300, #008300)',
};

const cacheColor = resultColors.HIT;
const originColor = resultColors.MISS;

export interface CacheChartsProps {
  hosts: readonly CacheHostStats[];
}

function ChartBlock({
  title,
  hint,
  children,
}: {
  title: string;
  hint: string;
  children: ReactNode;
}) {
  return (
    <Stack gap={4} component="figure" m={0}>
      <Text size="sm" fw={500} component="figcaption">
        {title}
      </Text>
      <Text size="xs" c="dimmed">
        {hint}
      </Text>
      {children}
    </Stack>
  );
}

/**
 * Per domain: the answers by X-Cache result (HIT, MISS, STALE...), the
 * bytes served from the cache and from the origin, and the hit ratio.
 * Every chart is an image for assistive technologies, named by its
 * title; the table of the domains next to it has the same figures.
 */
export default function CacheCharts({ hosts }: CacheChartsProps) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  // The results that occur, in their fixed order.
  const results = cacheResults.filter((r) => hosts.some((h) => (h.requests[r] ?? 0) > 0));
  const answers = hosts.map((h) => ({
    host: h.host,
    ...Object.fromEntries(cacheResults.map((r) => [r, h.requests[r] ?? 0])),
  }));
  const bytes = hosts.map((h) => ({ host: h.host, cache: h.bytesCache, origin: h.bytesOrigin }));
  const ratios = hosts
    .filter((h) => cacheableRequests(h.requests) > 0)
    .map((h) => ({ host: h.host, ratio: Math.round(h.hitRatio * 1000) / 10 }));
  const height = Math.max(140, hosts.length * 40 + 60);
  const yWidth = Math.min(200, Math.max(80, ...hosts.map((h) => h.host.length * 7)));
  return (
    <SimpleGrid cols={{ base: 1, lg: 3 }} spacing="lg">
      <ChartBlock title={t('cache.charts.answers')} hint={t('cache.charts.answersHint')}>
        <BarChart
          h={height}
          data={answers}
          dataKey="host"
          type="stacked"
          orientation="vertical"
          series={results.map((r) => ({ name: r, label: r, color: resultColors[r] }))}
          valueFormatter={(v) => formatNumber(v, locale)}
          withLegend
          legendProps={{ verticalAlign: 'bottom' }}
          gridAxis="x"
          strokeDasharray="0"
          tickLine="none"
          maxBarWidth={20}
          yAxisProps={{ width: yWidth }}
          tooltipAnimationDuration={0}
          role="img"
          aria-label={t('cache.charts.answers')}
          data-chart="answers"
        />
      </ChartBlock>
      <ChartBlock title={t('cache.charts.bytes')} hint={t('cache.charts.bytesHint')}>
        <BarChart
          h={height}
          data={bytes}
          dataKey="host"
          type="stacked"
          orientation="vertical"
          series={[
            { name: 'cache', label: t('cache.charts.fromCache'), color: cacheColor },
            { name: 'origin', label: t('cache.charts.fromOrigin'), color: originColor },
          ]}
          valueFormatter={(v) => formatBytes(v, locale)}
          withLegend
          legendProps={{ verticalAlign: 'bottom' }}
          gridAxis="x"
          strokeDasharray="0"
          tickLine="none"
          maxBarWidth={20}
          yAxisProps={{ width: yWidth }}
          tooltipAnimationDuration={0}
          role="img"
          aria-label={t('cache.charts.bytes')}
          data-chart="bytes"
        />
      </ChartBlock>
      <ChartBlock title={t('cache.charts.hitRatio')} hint={t('cache.charts.hitRatioHint')}>
        {ratios.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('cache.charts.noRequests')}
          </Text>
        ) : (
          <BarChart
            h={Math.max(140, ratios.length * 40 + 40)}
            data={ratios}
            dataKey="host"
            orientation="vertical"
            series={[{ name: 'ratio', label: t('cache.charts.ratio'), color: cacheColor }]}
            valueFormatter={(v) => `${String(v)}%`}
            xAxisProps={{ domain: [0, 100] }}
            gridAxis="x"
            strokeDasharray="0"
            tickLine="none"
            maxBarWidth={20}
            barProps={{ radius: [0, 4, 4, 0] }}
            yAxisProps={{ width: yWidth }}
            tooltipAnimationDuration={0}
            role="img"
            aria-label={t('cache.charts.hitRatio')}
            data-chart="hit-ratio"
          />
        )}
      </ChartBlock>
    </SimpleGrid>
  );
}
