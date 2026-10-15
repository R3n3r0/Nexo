/**
 * The line charts of the metrics page (D-MON-01). Charts are large
 * (Recharts): this module is only reached through a dynamic import, so it
 * stays out of the initial bundle and of the pages that do not draw them.
 */
import '@mantine/charts/styles.css';

import { LineChart } from '@mantine/charts';

import { chartRows, formatMetricValue, seriesColors } from '@/lib/monitoring';
import type { MetricPoint } from '@/services';

export interface MetricChartLine {
  /** A key unique in the chart (the name of the series or of the node). */
  key: string;
  label: string;
  points: readonly MetricPoint[];
}

export interface MetricChartProps {
  /** The accessible name of the chart (its title). */
  label: string;
  unit: string;
  lines: readonly MetricChartLine[];
  /** Seconds between two points: the ticks show seconds below a minute. */
  stepSeconds: number;
  locale?: string;
  height?: number;
}

/**
 * A line per series over time; null points are gaps (a node down, no
 * sample yet). The chart is an image for the assistive technologies,
 * named by its title; the latest values are written next to it.
 */
export default function MetricChart({
  label,
  unit,
  lines,
  stepSeconds,
  locale,
  height = 200,
}: MetricChartProps) {
  const rows = chartRows(lines.map((l) => ({ key: l.key, points: l.points })));
  const time = new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
    ...(stepSeconds < 60 ? { second: '2-digit' } : {}),
    hour12: false,
  });
  const data = rows.map((row) => ({ ...row, time: time.format(new Date(row.t * 1000)) }));
  return (
    <LineChart
      h={height}
      data={data}
      dataKey="time"
      series={lines.map((l, i) => ({
        name: l.key,
        label: l.label,
        color: seriesColors[i % seriesColors.length] ?? 'blue',
      }))}
      curveType="monotone"
      connectNulls={false}
      withDots={false}
      strokeWidth={1.75}
      withLegend={lines.length > 1}
      legendProps={{ verticalAlign: 'bottom' }}
      valueFormatter={(v) => formatMetricValue(v, unit, locale)}
      yAxisProps={{ width: 84, tickFormatter: (v: number) => formatMetricValue(v, unit, locale) }}
      xAxisProps={{ minTickGap: 32 }}
      gridAxis="xy"
      strokeDasharray="0"
      tickLine="none"
      tooltipAnimationDuration={0}
      role="img"
      aria-label={label}
      data-chart={label}
    />
  );
}
