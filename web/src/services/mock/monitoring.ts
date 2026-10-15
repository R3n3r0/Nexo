import { levelRank, parseGoDuration, statusMatches, traceMatches } from '@/lib/monitoring';

import type { ApiError } from '../errors';
import type { LiveStream, MonitoringService, StreamHandlers, StreamState } from '../interfaces';
import type {
  AuditEntry,
  LogEntry,
  LogFilter,
  MetricInfo,
  MetricPoint,
  MetricSeries,
  ServerInfo,
  TraceEvent,
  TraceFilter,
} from '../types';
import { clone, problem, type MockContext } from './support';

/** The series of the console of a Nexo server (GET /metrics). */
export const sampleMetrics: readonly MetricInfo[] = [
  { name: 's3_requests', description: 'S3 requests per second', unit: 'req/s', aggregation: 'sum' },
  {
    name: 's3_errors_4xx',
    description: 'S3 requests answered with a 4xx status per second',
    unit: 'req/s',
    aggregation: 'sum',
  },
  {
    name: 's3_errors_5xx',
    description: 'S3 requests answered with a 5xx status per second',
    unit: 'req/s',
    aggregation: 'sum',
  },
  {
    name: 's3_latency_avg',
    description: 'Average latency of the S3 requests',
    unit: 'seconds',
    aggregation: 'avg',
  },
  {
    name: 's3_latency_p50',
    description: 'Median latency of the S3 requests',
    unit: 'seconds',
    aggregation: 'max',
  },
  {
    name: 's3_latency_p95',
    description: '95th percentile of the latency of the S3 requests',
    unit: 'seconds',
    aggregation: 'max',
  },
  {
    name: 's3_latency_p99',
    description: '99th percentile of the latency of the S3 requests',
    unit: 'seconds',
    aggregation: 'max',
  },
  {
    name: 's3_rx_bytes',
    description: 'Bytes received by the S3 API per second',
    unit: 'bytes/s',
    aggregation: 'sum',
  },
  {
    name: 's3_tx_bytes',
    description: 'Bytes sent by the S3 API per second',
    unit: 'bytes/s',
    aggregation: 'sum',
  },
  {
    name: 's3_inflight',
    description: 'S3 requests being served',
    unit: 'requests',
    aggregation: 'sum',
  },
  {
    name: 'http_requests',
    description: 'HTTP requests of every listener per second',
    unit: 'req/s',
    aggregation: 'sum',
  },
  {
    name: 'drive_used_bytes',
    description: 'Used space of the local drives',
    unit: 'bytes',
    aggregation: 'sum',
  },
  {
    name: 'drive_free_bytes',
    description: 'Free space of the local drives',
    unit: 'bytes',
    aggregation: 'sum',
  },
  {
    name: 'drive_total_bytes',
    description: 'Capacity of the local drives',
    unit: 'bytes',
    aggregation: 'sum',
  },
  { name: 'drives_online', description: 'Local drives online', unit: 'drives', aggregation: 'sum' },
  {
    name: 'drives_offline',
    description: 'Local drives offline',
    unit: 'drives',
    aggregation: 'sum',
  },
  {
    name: 'drive_errors',
    description: 'Errors of the local drives per second',
    unit: 'errors/s',
    aggregation: 'sum',
  },
  {
    name: 'mrf_pending',
    description: 'Objects waiting in the healing queue',
    unit: 'objects',
    aggregation: 'sum',
  },
  {
    name: 'mrf_healed',
    description: 'Objects of the healing queue healed per second',
    unit: 'objects/s',
    aggregation: 'sum',
  },
  {
    name: 'heal_scanned',
    description: 'Items examined by the running healing jobs',
    unit: 'objects',
    aggregation: 'sum',
  },
  {
    name: 'scanner_scanning',
    description: 'Whether a scanner cycle is in progress',
    unit: 'boolean',
    aggregation: 'max',
  },
  {
    name: 'scanner_objects',
    description: 'Objects scanned by the last scanner cycle',
    unit: 'objects',
    aggregation: 'max',
  },
  {
    name: 'events_pending',
    description: 'Bucket notifications waiting for their targets',
    unit: 'events',
    aggregation: 'sum',
  },
  {
    name: 'rpc_requests',
    description: 'Internode calls per second',
    unit: 'req/s',
    aggregation: 'sum',
  },
  {
    name: 'rpc_errors',
    description: 'Failed internode calls per second',
    unit: 'errors/s',
    aggregation: 'sum',
  },
  {
    name: 'cpu_usage',
    description: 'CPU used by the server process',
    unit: 'cores',
    aggregation: 'sum',
  },
  {
    name: 'memory_bytes',
    description: 'Resident memory of the server process',
    unit: 'bytes',
    aggregation: 'sum',
  },
  {
    name: 'goroutines',
    description: 'Goroutines of the server process',
    unit: 'goroutines',
    aggregation: 'sum',
  },
];

/** Typical magnitudes of the series of the demo charts. */
const baselines: Readonly<Record<string, number>> = {
  s3_requests: 42,
  s3_errors_4xx: 1.5,
  s3_errors_5xx: 0.05,
  s3_latency_avg: 0.018,
  s3_latency_p50: 0.009,
  s3_latency_p95: 0.045,
  s3_latency_p99: 0.12,
  s3_rx_bytes: 8 * 1024 ** 2,
  s3_tx_bytes: 21 * 1024 ** 2,
  s3_inflight: 4,
  http_requests: 55,
  drive_used_bytes: 194 * 1024 ** 3,
  drive_free_bytes: 318 * 1024 ** 3,
  drive_total_bytes: 512 * 1024 ** 3,
  drives_online: 1,
  drives_offline: 0,
  drive_errors: 0,
  mrf_pending: 3,
  mrf_healed: 0.2,
  heal_scanned: 0,
  scanner_scanning: 0,
  scanner_objects: 12840,
  events_pending: 0,
  rpc_requests: 120,
  rpc_errors: 0.1,
  cpu_usage: 0.35,
  memory_bytes: 310 * 1024 ** 2,
  goroutines: 240,
};

const maxReplay = 10_000;

/** A sample of the audit log: requests of root and of a user. */
export function sampleAuditEntries(now: Date): AuditEntry[] {
  const at = (minutesAgo: number) => new Date(now.getTime() - minutesAgo * 60_000).toISOString();
  return [
    {
      time: at(1),
      node: 'local',
      type: 's3',
      requestId: '17F0A1',
      api: 'PutObject',
      method: 'PUT',
      path: '/photos/2026/beach.jpg',
      bucket: 'photos',
      object: '2026/beach.jpg',
      statusCode: 200,
      durationMs: 18.4,
      rx: 482_133,
      tx: 0,
      remoteHost: '10.0.0.12',
      userAgent: 'aws-sdk-go-v2',
      accessKey: 'nexoadmin',
    },
    {
      time: at(3),
      node: 'local',
      type: 's3',
      requestId: '17F09C',
      api: 'GetObject',
      method: 'GET',
      path: '/backups/db.tar.gz',
      bucket: 'backups',
      object: 'db.tar.gz',
      statusCode: 403,
      durationMs: 1.2,
      rx: 0,
      tx: 243,
      remoteHost: '10.0.0.40',
      userAgent: 'curl/8.5',
      accessKey: 'alice-key',
      parentUser: 'alice',
      error: 'Access Denied.',
    },
    {
      time: at(9),
      node: 'local',
      type: 'admin',
      requestId: '17F088',
      api: 'POST /users',
      method: 'POST',
      path: '/api/v1/users',
      statusCode: 201,
      durationMs: 6.3,
      rx: 96,
      tx: 180,
      remoteHost: '10.0.0.12',
      userAgent: 'Mozilla/5.0',
      accessKey: 'nexoadmin',
    },
    {
      time: at(32),
      node: 'local',
      type: 's3',
      requestId: '17F011',
      api: 'DeleteObject',
      method: 'DELETE',
      path: '/photos/old.png',
      bucket: 'photos',
      object: 'old.png',
      statusCode: 204,
      durationMs: 3.1,
      rx: 0,
      tx: 0,
      remoteHost: '10.0.0.40',
      userAgent: 'nexoctl/0.1.0',
      accessKey: 'alice-key',
      parentUser: 'alice',
    },
  ];
}

export interface MockMonitoringOptions {
  /** Emit synthetic trace events and log entries (the demo mode). */
  liveDemo?: boolean;
  /** The audit records of the mock (default: sampleAuditEntries). */
  auditEntries?: AuditEntry[];
}

/** The mock with the controls the tests drive the live streams with. */
export interface MockMonitoringService extends MonitoringService {
  /** Delivers a request to the open trace streams whose filter it passes. */
  emitTrace(event: TraceEvent): void;
  /** Delivers a log entry to the open log streams (and keeps it for the replays). */
  emitLog(entry: LogEntry): void;
  /** Reports dropped events to every open stream. */
  emitDropped(total: number): void;
  /** Breaks the connections: the streams reconnect, then open again. */
  disconnect(): void;
  /** The open streams (trace and logs). */
  openStreams(): number;
  /** The filters of the open trace streams (latest first). */
  traceFilters(): TraceFilter[];
}

interface Subscriber<T, F> {
  filter: F;
  handlers: StreamHandlers<T>;
}

export interface MockMonitoringDeps {
  /** The deployment whose nodes the series and the streams name. */
  info: () => ServerInfo;
  /** Whether the caller may perform an action. */
  allowed: (action: string) => boolean;
}

/** The nodes of the deployment as the monitoring endpoints name them. */
function nodesOf(info: ServerInfo): { name: string; online: boolean }[] {
  if (info.mode !== 'distributed') return [{ name: 'local', online: true }];
  return info.nodes.map((n) => ({ name: n.endpoint, online: n.state === 'online' }));
}

/** The mock of the monitoring: deterministic series, controllable streams. */
export function createMockMonitoringService(
  ctx: MockContext,
  deps: MockMonitoringDeps,
  options: MockMonitoringOptions = {},
): MockMonitoringService {
  const traces = new Set<Subscriber<TraceEvent, TraceFilter>>();
  const logs = new Set<Subscriber<LogEntry, LogFilter>>();
  const history: LogEntry[] = [];
  const audit = options.auditEntries ?? sampleAuditEntries(ctx.now());

  const refuse = <T>(action: string, handlers: StreamHandlers<T>): LiveStream | undefined => {
    if (deps.allowed(action)) return undefined;
    const error: ApiError = problem(403, 'access_denied', `no policy allows ${action}`);
    queueMicrotask(() => {
      handlers.onState?.('closed', error);
    });
    return { close: () => undefined };
  };

  const subscribe = <T, F>(
    set: Set<Subscriber<T, F>>,
    filter: F,
    handlers: StreamHandlers<T>,
    replay: () => T[],
  ): LiveStream => {
    const sub: Subscriber<T, F> = { filter: clone(filter), handlers };
    let closed = false;
    handlers.onState?.('connecting');
    queueMicrotask(() => {
      if (closed) return;
      set.add(sub);
      handlers.onState?.('open');
      for (const item of replay()) handlers.onItem(clone(item));
    });
    return {
      close() {
        closed = true;
        set.delete(sub);
      },
    };
  };

  const logMatches = (entry: LogEntry, filter: LogFilter) =>
    ((filter.nodes ?? []).length === 0 || (filter.nodes ?? []).includes(entry.node)) &&
    levelRank(entry.level) >= levelRank(filter.level ?? 'debug');

  const service: MockMonitoringService = {
    async metrics() {
      await ctx.delay();
      if (!deps.allowed('admin:Prometheus') && !deps.allowed('admin:ServerInfo')) {
        throw problem(403, 'access_denied', 'no policy allows admin:Prometheus');
      }
      return clone([...sampleMetrics]);
    },
    async queryMetric(query) {
      await ctx.delay();
      if (!deps.allowed('admin:Prometheus') && !deps.allowed('admin:ServerInfo')) {
        throw problem(403, 'access_denied', 'no policy allows admin:Prometheus');
      }
      const metric = sampleMetrics.find((m) => m.name === query.metric);
      if (metric === undefined) {
        throw problem(404, 'not_found', `unknown metric "${query.metric}"`);
      }
      const rangeMs = parseGoDuration(query.range ?? '1h') ?? 3_600_000;
      if (rangeMs <= 0 || rangeMs > 24 * 3_600_000) {
        throw problem(400, 'invalid_argument', 'range must be a duration of at most 24h');
      }
      const stepSec = Math.max(
        10,
        Math.ceil((parseGoDuration(query.step ?? '') ?? rangeMs / 360) / 10_000) * 10,
      );
      const endSec = Math.floor(
        (query.end !== undefined ? Date.parse(query.end) : ctx.now().getTime()) / 1000,
      );
      const startSec = Math.floor((endSec - rangeMs / 1000) / stepSec) * stepSec;
      const times: number[] = [];
      for (let t = startSec; t <= endSec; t += stepSec) times.push(t);
      const wanted = query.nodes ?? [];
      const nodes = nodesOf(deps.info()).filter(
        (n) => wanted.length === 0 || wanted.includes(n.name),
      );
      const base = baselines[metric.name] ?? 1;
      const series = nodes.map((node, i) => ({
        node: node.name,
        points: times.map((t): MetricPoint => ({
          t,
          v: node.online ? wave(base, t, i, metric.unit) : null,
        })),
        ...(node.online ? {} : { error: 'node offline' }),
      }));
      const cluster = times.map((t, j): MetricPoint => {
        const values = series
          .map((s) => s.points[j]?.v)
          .filter((v): v is number => v !== null && v !== undefined);
        if (values.length === 0) return { t, v: null };
        const sum = values.reduce((a, b) => a + b, 0);
        const v =
          metric.aggregation === 'avg'
            ? sum / values.length
            : metric.aggregation === 'max'
              ? Math.max(...values)
              : sum;
        return { t, v };
      });
      const result: MetricSeries = {
        metric: clone(metric),
        start: new Date(startSec * 1000).toISOString(),
        end: new Date(endSec * 1000).toISOString(),
        stepSeconds: stepSec,
        nodes: series,
        cluster,
      };
      return result;
    },
    trace(filter, handlers) {
      return (
        refuse('admin:ServerTrace', handlers) ??
        subscribe(traces, filter, handlers, () => [] as TraceEvent[])
      );
    },
    logs(filter, handlers) {
      const last = Math.min(Math.max(filter.last ?? 0, 0), maxReplay);
      return (
        refuse('admin:ConsoleLog', handlers) ??
        subscribe(logs, filter, handlers, () =>
          last > 0 ? history.filter((e) => logMatches(e, filter)).slice(-last) : [],
        )
      );
    },
    async audit(query) {
      await ctx.delay();
      if (!deps.allowed('admin:ConsoleLog')) {
        throw problem(403, 'access_denied', 'no policy allows admin:ConsoleLog');
      }
      const limit = Math.min(query.limit ?? 100, 1000);
      const since = query.since !== undefined ? Date.parse(query.since) : undefined;
      const until = query.until !== undefined ? Date.parse(query.until) : undefined;
      const user = query.user?.trim();
      const api = query.api?.trim().toLowerCase();
      const out = audit
        .filter((e) => {
          const time = Date.parse(e.time);
          return (
            ((query.nodes ?? []).length === 0 || (query.nodes ?? []).includes(e.node ?? '')) &&
            (api === undefined || api === '' || e.api.toLowerCase() === api) &&
            (query.bucket === undefined || query.bucket === '' || e.bucket === query.bucket) &&
            (user === undefined || user === '' || e.accessKey === user || e.parentUser === user) &&
            (query.type === undefined || e.type === query.type) &&
            statusMatches(e.statusCode, query.statuses) &&
            (query.errorsOnly !== true || e.statusCode >= 400 || (e.error ?? '') !== '') &&
            (since === undefined || time >= since) &&
            (until === undefined || time <= until) &&
            (query.requestId === undefined ||
              query.requestId === '' ||
              e.requestId === query.requestId)
          );
        })
        .sort((a, b) => Date.parse(b.time) - Date.parse(a.time))
        .slice(0, limit);
      return clone(out);
    },
    emitTrace(event) {
      for (const sub of [...traces]) {
        if (traceMatches(event, sub.filter)) sub.handlers.onItem(clone(event));
      }
    },
    emitLog(entry) {
      history.push(clone(entry));
      if (history.length > maxReplay) history.shift();
      for (const sub of [...logs]) {
        if (logMatches(entry, sub.filter)) sub.handlers.onItem(clone(entry));
      }
    },
    emitDropped(total) {
      for (const sub of [...traces, ...logs]) sub.handlers.onDropped?.(total);
    },
    disconnect() {
      const subs = [...traces, ...logs];
      const states: StreamState[] = ['reconnecting', 'open'];
      for (const state of states) {
        for (const sub of subs) sub.handlers.onState?.(state);
      }
    },
    openStreams() {
      return traces.size + logs.size;
    },
    traceFilters() {
      return [...traces].reverse().map((s) => clone(s.filter));
    },
  };

  if (options.liveDemo === true) startDemo(ctx, deps, service);
  return service;
}

/** A smooth deterministic wave around a baseline (the demo charts). */
function wave(base: number, t: number, node: number, unit: string): number {
  if (unit === 'drives' || unit === 'boolean' || unit === 'bytes') {
    return unit === 'bytes' ? base * (1 + node * 0.05) : base;
  }
  const v = base * (1 + 0.35 * Math.sin(t / 900 + node) + 0.15 * Math.sin(t / 97 + node * 2));
  return Math.max(0, v);
}

const demoApis = [
  ['PutObject', 'PUT', 200],
  ['GetObject', 'GET', 200],
  ['ListObjectsV2', 'GET', 200],
  ['HeadObject', 'HEAD', 404],
  ['GetObject', 'GET', 403],
  ['DeleteObject', 'DELETE', 204],
] as const;

/** Emits a request and a log line every couple of seconds (demo mode). */
function startDemo(ctx: MockContext, deps: MockMonitoringDeps, service: MockMonitoringService) {
  let n = 0;
  setInterval(() => {
    if (service.openStreams() === 0) return;
    n++;
    const nodes = nodesOf(deps.info()).filter((x) => x.online);
    const node = nodes[n % Math.max(nodes.length, 1)]?.name ?? 'local';
    const [api, method, status] = demoApis[n % demoApis.length] ?? demoApis[0];
    const bucket = n % 3 === 0 ? 'backups' : 'photos';
    service.emitTrace({
      time: ctx.nowIso(),
      node,
      type: 's3',
      api,
      method,
      path: `/${bucket}/object-${String(n % 17)}.bin`,
      status,
      durationMs: 2 + ((n * 37) % 90) / 3,
      inputBytes: method === 'PUT' ? 65_536 : 0,
      outputBytes: method === 'GET' && status === 200 ? 131_072 : 0,
      requestId: `DEMO${String(n).padStart(6, '0')}`,
      clientIp: '10.0.0.12',
      user: 'nexoadmin',
      bucket,
      ...(status >= 400 ? { error: status === 404 ? 'Not Found' : 'Access Denied.' } : {}),
    });
    if (n % 2 === 0) {
      service.emitLog({
        time: ctx.nowIso(),
        node,
        level: n % 10 === 0 ? 'WARN' : 'INFO',
        message: n % 10 === 0 ? 'heal: object queued' : 'scanner: cycle progress',
        attrs: { bucket, objects: String(n * 13) },
      });
    }
  }, 1500);
}
