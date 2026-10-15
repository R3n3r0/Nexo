/**
 * Helpers of the monitoring pages (D-MON-01..03): durations, statuses,
 * levels, the units of the series and the layout of the charts.
 */
import type { LogLevel, MetricInfo, MetricSeries, TraceEvent, TraceFilter } from '@/services';

import { formatBytes, formatCompact } from './format';

const durationUnits: Readonly<Record<string, number>> = {
  ns: 1e-6,
  us: 1e-3,
  µs: 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
};

/**
 * Parses a Go duration ("500ms", "1.5s", "1m30s") into milliseconds;
 * undefined when it is not one. A bare "0" is zero.
 */
export function parseGoDuration(text: string): number | undefined {
  const value = text.trim();
  if (value === '0') return 0;
  if (value === '') return undefined;
  const pattern = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|ms|s|m|h)/gy;
  let total = 0;
  let consumed = 0;
  for (const match of value.matchAll(pattern)) {
    const [whole, amount, unit] = match;
    if (match.index !== consumed || amount === undefined || unit === undefined) return undefined;
    total += Number(amount) * (durationUnits[unit] ?? 0);
    consumed += whole.length;
  }
  return consumed === value.length && consumed > 0 ? total : undefined;
}

/** Whether a status matches a filter of codes ("404") and classes ("5xx"); empty matches all. */
export function statusMatches(status: number, statuses: readonly string[] | undefined): boolean {
  if (statuses === undefined || statuses.length === 0) return true;
  return statuses.some((s) => {
    const value = s.trim().toLowerCase();
    if (/^[1-5]xx$/.test(value)) return Math.floor(status / 100) === Number(value[0]);
    return Number(value) === status;
  });
}

/** A status filter value: a code (100-599) or a class (1xx-5xx). */
export function isStatusFilter(value: string): boolean {
  const v = value.trim().toLowerCase();
  if (/^[1-5]xx$/.test(v)) return true;
  const n = Number(v);
  return /^\d{3}$/.test(v) && n >= 100 && n <= 599;
}

/** Whether a trace event passes a filter (the mock of the server filter). */
export function traceMatches(event: TraceEvent, filter: TraceFilter): boolean {
  const lower = (values: readonly string[] | undefined) =>
    (values ?? []).map((v) => v.toLowerCase());
  const types = filter.types ?? [];
  if (types.length > 0 ? !types.includes(event.type) : event.type === 'rpc') return false;
  const nodes = filter.nodes ?? [];
  if (nodes.length > 0 && !nodes.includes(event.node)) return false;
  const apis = lower(filter.apis);
  if (apis.length > 0 && !apis.includes((event.api ?? '').toLowerCase())) return false;
  const buckets = filter.buckets ?? [];
  if (buckets.length > 0 && !buckets.includes(event.bucket ?? '')) return false;
  if (!statusMatches(event.status, filter.statuses)) return false;
  if (filter.errorsOnly === true && event.status < 400 && (event.error ?? '') === '') return false;
  const threshold = filter.threshold !== undefined ? parseGoDuration(filter.threshold) : undefined;
  return threshold === undefined || event.durationMs >= threshold;
}

export const logLevels: readonly LogLevel[] = ['DEBUG', 'INFO', 'WARN', 'ERROR'];

/** The rank of a level (DEBUG 0 ... ERROR 3); unknown levels rank as INFO. */
export function levelRank(level: string): number {
  const i = logLevels.indexOf(level.toUpperCase() as LogLevel);
  return i < 0 ? 1 : i;
}

/** Formats a duration in milliseconds: 0.42 ms, 12 ms, 1.2 s. */
export function formatMs(ms: number, locale?: string): string {
  if (!Number.isFinite(ms)) return '—';
  if (ms >= 1000) {
    return `${new Intl.NumberFormat(locale, { maximumFractionDigits: 2 }).format(ms / 1000)} s`;
  }
  const digits = ms >= 100 ? 0 : ms >= 10 ? 1 : 2;
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: digits }).format(ms)} ms`;
}

/** Formats a value of a series in its unit (GET /metrics). */
export function formatMetricValue(value: number | null | undefined, unit: string, locale?: string) {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—';
  switch (unit) {
    case 'bytes':
      return formatBytes(value, locale);
    case 'bytes/s':
      return `${formatBytes(value, locale)}/s`;
    case 'seconds':
      return formatMs(value * 1000, locale);
    case 'boolean':
      return value > 0 ? '1' : '0';
    case 'cores':
      return new Intl.NumberFormat(locale, { maximumFractionDigits: 2 }).format(value);
    default: {
      const rate = unit.endsWith('/s');
      const text =
        rate && Math.abs(value) < 100
          ? new Intl.NumberFormat(locale, { maximumFractionDigits: 2 }).format(value)
          : formatCompact(Math.round(value), locale);
      return `${text} ${unit}`;
    }
  }
}

/** The charts of the metrics page. */
export type MetricChartId =
  | 'requests'
  | 'errors'
  | 'latency'
  | 'bandwidth'
  | 'disk'
  | 'drives'
  | 'healing'
  | 'internode'
  | 'cpu'
  | 'memory';

/** The series the charts draw (each has a short label in the locales). */
export const chartedSeries = [
  's3_requests',
  'http_requests',
  's3_errors_4xx',
  's3_errors_5xx',
  's3_latency_avg',
  's3_latency_p50',
  's3_latency_p95',
  's3_latency_p99',
  's3_rx_bytes',
  's3_tx_bytes',
  'drive_used_bytes',
  'drive_free_bytes',
  'drives_online',
  'drives_offline',
  'mrf_pending',
  'heal_scanned',
  'scanner_objects',
  'rpc_requests',
  'rpc_errors',
  'cpu_usage',
  'memory_bytes',
] as const;
export type ChartedSeries = (typeof chartedSeries)[number];

/** Whether a series has a short label in the locales. */
export function isChartedSeries(name: string): name is ChartedSeries {
  return (chartedSeries as readonly string[]).includes(name);
}

/** A chart of the metrics page: the series it draws, all in one unit. */
export interface MetricChartSpec {
  id: MetricChartId;
  metrics: readonly ChartedSeries[];
  /** Only meaningful with more than one node. */
  clusterOnly?: boolean;
}

/** The charts of the metrics page, in display order (D-MON-01). */
export const metricCharts: readonly MetricChartSpec[] = [
  { id: 'requests', metrics: ['s3_requests', 'http_requests'] },
  { id: 'errors', metrics: ['s3_errors_4xx', 's3_errors_5xx'] },
  { id: 'latency', metrics: ['s3_latency_p50', 's3_latency_p95', 's3_latency_p99'] },
  { id: 'bandwidth', metrics: ['s3_rx_bytes', 's3_tx_bytes'] },
  { id: 'disk', metrics: ['drive_used_bytes', 'drive_free_bytes'] },
  { id: 'drives', metrics: ['drives_online', 'drives_offline'] },
  { id: 'healing', metrics: ['mrf_pending', 'heal_scanned', 'scanner_objects'] },
  { id: 'internode', metrics: ['rpc_requests', 'rpc_errors'], clusterOnly: true },
  { id: 'cpu', metrics: ['cpu_usage'] },
  { id: 'memory', metrics: ['memory_bytes'] },
];

/** The sizes of the buffer of a live view (trace, logs). */
export const bufferSizes = [100, 500, 1000, 5000] as const;

/** Time ranges of the charts and the refresh intervals offered. */
export const metricRanges = ['15m', '1h', '6h', '24h'] as const;
export type MetricRange = (typeof metricRanges)[number];
export const refreshIntervals = ['off', '10s', '30s', '1m'] as const;
export type RefreshInterval = (typeof refreshIntervals)[number];

/** The interval in milliseconds of a refresh choice; false when off. */
export function refreshMs(interval: RefreshInterval): number | false {
  const ms = interval === 'off' ? undefined : parseGoDuration(interval);
  return ms === undefined ? false : ms;
}

/**
 * Categorical colours of the series of a chart, in a fixed order, stepped
 * for the light and the dark surfaces (the cache charts use the same).
 */
export const seriesColors: readonly string[] = [
  'light-dark(#2a78d6, #3987e5)',
  'light-dark(#eb6834, #d95926)',
  'light-dark(#1baf7a, #199e70)',
  'light-dark(#8a5cd0, #a67ee8)',
  'light-dark(#eda100, #c98500)',
  'light-dark(#e87ba4, #d55181)',
];

/** A row of a chart: the time and one value per series (null: no sample). */
export type ChartRow = { t: number } & Record<string, number | null>;

/**
 * Merges the points of several series into the rows of a chart, keyed by
 * the time of the points (the series of one query share it): pick selects
 * the points of a series (the cluster or one node).
 */
export function chartRows(
  series: readonly { key: string; points: readonly { t: number; v?: number | null }[] }[],
): ChartRow[] {
  const rows = new Map<number, ChartRow>();
  for (const s of series) {
    for (const p of s.points) {
      let row = rows.get(p.t);
      if (row === undefined) {
        row = { t: p.t };
        rows.set(p.t, row);
      }
      row[s.key] = p.v ?? null;
    }
  }
  const out = [...rows.values()].sort((a, b) => a.t - b.t);
  for (const row of out) {
    for (const s of series) {
      if (!(s.key in row)) row[s.key] = null;
    }
  }
  return out;
}

/** The last value of the points that has one. */
export function lastValue<P extends { v?: number | null }>(points: readonly P[]): number | null {
  for (let i = points.length - 1; i >= 0; i--) {
    const v = points[i]?.v;
    if (v !== null && v !== undefined) return v;
  }
  return null;
}

/** Whether any point of the series has a value. */
export function hasValues(series: MetricSeries | undefined): boolean {
  return (series?.cluster ?? []).some((p) => p.v !== null && p.v !== undefined);
}

/** The unit of a chart: the one of its first known series. */
export function chartUnit(spec: MetricChartSpec, known: readonly MetricInfo[]): string {
  for (const name of spec.metrics) {
    const info = known.find((m) => m.name === name);
    if (info !== undefined) return info.unit;
  }
  return '';
}

/** The colour of an HTTP status: green 2xx, blue 3xx, orange 4xx, red 5xx. */
export function statusColor(status: number): string {
  if (status >= 500) return 'red';
  if (status >= 400) return 'orange';
  if (status >= 300) return 'blue';
  return 'teal';
}

/** A time with milliseconds in the locale of the console. */
export function formatClock(iso: string, locale?: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    fractionalSecondDigits: 3,
    hour12: false,
  }).format(date);
}

/** Days before the expiry of a certificate when it is flagged (the server warns at 21). */
export const certificateWarningDays = 21;

/** The state of a certificate at a time: valid, expiring (with the days left), expired, not yet valid. */
export function certificateState(
  cert: { notBefore: string; notAfter: string },
  now: Date,
): { state: 'valid' | 'expiring' | 'expired' | 'notYetValid'; days: number } {
  const notAfter = Date.parse(cert.notAfter);
  const notBefore = Date.parse(cert.notBefore);
  const days = Math.floor((notAfter - now.getTime()) / 86_400_000);
  if (notAfter <= now.getTime()) return { state: 'expired', days };
  if (notBefore > now.getTime()) return { state: 'notYetValid', days };
  if (days < certificateWarningDays) return { state: 'expiring', days };
  return { state: 'valid', days };
}
