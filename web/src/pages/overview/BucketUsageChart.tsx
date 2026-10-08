/**
 * Storage per bucket (top 10) as a horizontal bar chart. Charts are large
 * (Recharts): this module is only reached through a dynamic import from
 * the overview, so it stays out of the initial bundle.
 */
import '@mantine/charts/styles.css';

import { BarChart } from '@mantine/charts';
import { useTranslation } from 'react-i18next';

import { formatBytes } from '@/lib/format';

export interface BucketUsageDatum {
  bucket: string;
  size: number;
}

export interface BucketUsageChartProps {
  data: readonly BucketUsageDatum[];
}

/** One series, one hue: no legend (the section title names it), tooltip on hover. */
export default function BucketUsageChart({ data }: BucketUsageChartProps) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const format = (value: number) => formatBytes(value, locale);
  return (
    <BarChart
      h={Math.max(160, data.length * 36 + 40)}
      data={[...data]}
      dataKey="bucket"
      orientation="vertical"
      series={[{ name: 'size', label: t('overview.charts.size'), color: 'blue.6' }]}
      valueFormatter={format}
      gridAxis="x"
      strokeDasharray="0"
      tickLine="none"
      maxBarWidth={24}
      barProps={{ radius: [0, 4, 4, 0] }}
      yAxisProps={{ width: 110 }}
      tooltipAnimationDuration={0}
      // A chart is an image for assistive technologies; its name is the title.
      role="img"
      aria-label={t('overview.charts.perBucket')}
    />
  );
}
