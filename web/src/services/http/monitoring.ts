import type { MonitoringService } from '../interfaces';
import type {
  AuditEntry,
  AuditQuery,
  LogFilter,
  MetricInfo,
  MetricSeries,
  TraceFilter,
} from '../types';
import type { HttpClient, QueryParams } from './client';
import { browserEventSource, type EventSourceFactory, openEventStream } from './stream';

export interface HttpMonitoringOptions {
  /** Opens the Server-Sent Events streams (default: the browser EventSource). */
  eventSource?: EventSourceFactory;
}

/** A comma-separated list parameter, absent when empty. */
function list(values: readonly string[] | undefined): string | undefined {
  const kept = (values ?? []).map((v) => v.trim()).filter((v) => v !== '');
  return kept.length > 0 ? kept.join(',') : undefined;
}

/** The query of GET /trace. */
export function traceQuery(filter: TraceFilter): QueryParams {
  const threshold = filter.threshold?.trim();
  return {
    node: list(filter.nodes),
    type: list(filter.types),
    api: list(filter.apis),
    bucket: list(filter.buckets),
    status: list(filter.statuses),
    errors: filter.errorsOnly === true ? true : undefined,
    threshold: threshold !== undefined && threshold !== '' ? threshold : undefined,
  };
}

/** The query of GET /logs. */
export function logsQuery(filter: LogFilter): QueryParams {
  return {
    node: list(filter.nodes),
    level: filter.level,
    last: filter.last !== undefined && filter.last > 0 ? filter.last : undefined,
  };
}

/** The query of GET /audit. */
export function auditQuery(query: AuditQuery): QueryParams {
  const text = (v: string | undefined) =>
    v !== undefined && v.trim() !== '' ? v.trim() : undefined;
  return {
    node: list(query.nodes),
    api: text(query.api),
    bucket: text(query.bucket),
    user: text(query.user),
    type: query.type,
    status: list(query.statuses),
    errors: query.errorsOnly === true ? true : undefined,
    since: text(query.since),
    until: text(query.until),
    requestId: text(query.requestId),
    limit: query.limit,
  };
}

/**
 * MonitoringService over GET /metrics, GET /metrics/query, GET /audit and
 * the Server-Sent Events of GET /trace and GET /logs.
 */
export function createHttpMonitoringService(
  client: HttpClient,
  options: HttpMonitoringOptions = {},
): MonitoringService {
  const eventSource = options.eventSource ?? browserEventSource;
  return {
    async metrics() {
      const body = await client.request<{ items: MetricInfo[] }>('/metrics');
      return body.items;
    },
    queryMetric(query) {
      return client.request<MetricSeries>('/metrics/query', {
        query: {
          metric: query.metric,
          range: query.range,
          step: query.step,
          end: query.end,
          node: list(query.nodes),
        },
      });
    },
    trace(filter, handlers) {
      return openEventStream({
        client,
        eventSource,
        path: '/trace',
        query: traceQuery(filter),
        event: 'trace',
        handlers,
      });
    },
    logs(filter, handlers) {
      return openEventStream({
        client,
        eventSource,
        path: '/logs',
        query: logsQuery(filter),
        event: 'log',
        handlers,
      });
    },
    async audit(query) {
      const body = await client.request<{ items: AuditEntry[] }>('/audit', {
        query: auditQuery(query),
      });
      return body.items;
    },
  };
}
