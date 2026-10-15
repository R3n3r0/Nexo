import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { api, problemResponse } from '@/test/handlers';
import { server } from '@/test/server';

import { createAuthEventEmitter } from '../events';
import type { StreamState } from '../interfaces';
import type { TraceEvent } from '../types';
import { createHttpClient } from './client';
import { createHttpServices } from './index';
import { createHttpMonitoringService } from './monitoring';
import type { EventSourceLike } from './stream';

/** A scriptable EventSource (jsdom has none). */
class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = [];
  readyState = 0;
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  closed = false;
  private listeners = new Map<string, ((event: MessageEvent<string>) => void)[]>();

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: MessageEvent<string>) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  close() {
    this.closed = true;
    this.readyState = 2;
  }

  open() {
    this.readyState = 1;
    this.onopen?.(new Event('open'));
  }

  emit(type: string, data: unknown) {
    const event = new MessageEvent(type, { data: JSON.stringify(data) });
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }

  /** A network failure the browser retries by itself. */
  drop() {
    this.readyState = 0;
    this.onerror?.(new Event('error'));
  }

  /** An answer that is not a stream: the browser gives up. */
  fail() {
    this.readyState = 2;
    this.onerror?.(new Event('error'));
  }

  static last(): FakeEventSource {
    const es = FakeEventSource.instances.at(-1);
    if (es === undefined) throw new Error('no EventSource opened');
    return es;
  }
}

const event: TraceEvent = {
  time: '2026-10-15T10:00:00Z',
  node: 'local',
  type: 's3',
  api: 'PutObject',
  method: 'PUT',
  path: '/photos/a.jpg',
  status: 200,
  durationMs: 12.5,
  inputBytes: 1024,
  outputBytes: 0,
};

/** The monitoring service itself (the bundle loads it lazily) and its auth events. */
function services() {
  const authEvents = createAuthEventEmitter();
  const client = createHttpClient({
    baseUrl: '/api/v1',
    onUnauthorized: () => {
      authEvents.emitUnauthorized();
    },
  });
  const monitoring = createHttpMonitoringService(client, {
    eventSource: (url) => new FakeEventSource(url),
  });
  return { monitoring, authEvents };
}

afterEach(() => {
  FakeEventSource.instances = [];
  vi.useRealTimers();
});

describe('HttpMonitoringService streams', () => {
  it('opens GET /trace with the filter and delivers the events and the drops', () => {
    const { monitoring } = services();
    const items: TraceEvent[] = [];
    const states: StreamState[] = [];
    let dropped = 0;
    const stream = monitoring.trace(
      {
        nodes: ['n1:9000', 'n2:9000'],
        types: ['s3', 'admin'],
        apis: ['PutObject'],
        buckets: [],
        statuses: ['5xx', '404'],
        errorsOnly: true,
        threshold: ' 250ms ',
      },
      {
        onItem: (e) => items.push(e),
        onDropped: (n) => (dropped = n),
        onState: (s) => states.push(s),
      },
    );
    const es = FakeEventSource.last();
    const url = new URL(es.url);
    expect(url.pathname).toBe('/api/v1/trace');
    expect(Object.fromEntries(url.searchParams)).toEqual({
      node: 'n1:9000,n2:9000',
      type: 's3,admin',
      api: 'PutObject',
      status: '5xx,404',
      errors: 'true',
      threshold: '250ms',
    });
    es.open();
    es.emit('trace', event);
    es.emit('dropped', { dropped: 7 });
    es.emit('trace', 'not an event');
    expect(items).toHaveLength(2);
    expect(items[0]).toEqual(event);
    expect(dropped).toBe(7);
    expect(states).toEqual(['connecting', 'open']);

    stream.close();
    expect(es.closed).toBe(true);
    es.emit('trace', event);
    expect(items).toHaveLength(2);
  });

  it('opens GET /logs with the level and the replay', () => {
    const { monitoring } = services();
    monitoring.logs({ nodes: [], level: 'warn', last: 50 }, { onItem: () => undefined });
    const url = new URL(FakeEventSource.last().url);
    expect(url.pathname).toBe('/api/v1/logs');
    expect(Object.fromEntries(url.searchParams)).toEqual({ level: 'warn', last: '50' });
  });

  it('reports the retries of the browser after a network failure', () => {
    const { monitoring } = services();
    const states: StreamState[] = [];
    monitoring.trace({}, { onItem: () => undefined, onState: (s) => states.push(s) });
    const es = FakeEventSource.last();
    es.open();
    es.drop();
    es.open();
    expect(states).toEqual(['connecting', 'open', 'reconnecting', 'open']);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('closes with the problem of a refused stream (403)', async () => {
    server.use(
      http.get(api('/trace'), () =>
        problemResponse(403, 'access_denied', 'no policy allows admin:ServerTrace'),
      ),
    );
    const { monitoring } = services();
    const closed = new Promise<unknown>((resolve) => {
      monitoring.trace(
        {},
        {
          onItem: () => undefined,
          onState: (s, error) => {
            if (s === 'closed') resolve(error);
          },
        },
      );
    });
    FakeEventSource.last().fail();
    await expect(closed).resolves.toMatchObject({ status: 403, code: 'access_denied' });
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('signs the console out when the session expired (401)', async () => {
    server.use(http.get(api('/logs'), () => problemResponse(401, 'unauthenticated')));
    const s = services();
    const unauthorized = new Promise<void>((resolve) => {
      s.authEvents.onUnauthorized(resolve);
    });
    s.monitoring.logs({}, { onItem: () => undefined });
    FakeEventSource.last().fail();
    await unauthorized;
  });

  it('reconnects with a growing delay after a server error', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let calls = 0;
    server.use(
      http.get(api('/trace'), () => {
        calls++;
        return problemResponse(502, 'bad_gateway');
      }),
    );
    const { monitoring } = services();
    const states: StreamState[] = [];
    const stream = monitoring.trace(
      {},
      { onItem: () => undefined, onState: (s) => states.push(s) },
    );
    FakeEventSource.last().fail();
    await vi.waitFor(() => {
      expect(calls).toBe(1);
      expect(states.at(-1)).toBe('reconnecting');
    });
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances).toHaveLength(2);
    FakeEventSource.last().open();
    expect(states.at(-1)).toBe('open');

    // The stream works again behind the failure: reopened without error.
    server.use(
      http.get(
        api('/trace'),
        () =>
          new HttpResponse('retry: 3000\n\n', { headers: { 'Content-Type': 'text/event-stream' } }),
      ),
    );
    FakeEventSource.last().fail();
    await vi.advanceTimersByTimeAsync(1000);
    await vi.waitFor(() => {
      expect(FakeEventSource.instances).toHaveLength(3);
    });
    stream.close();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakeEventSource.instances).toHaveLength(3);
  });
});

describe('the monitoring of the HTTP bundle', () => {
  it('loads the implementation on first use and opens the stream then', async () => {
    const { monitoring } = createHttpServices({
      baseUrl: '/api/v1',
      eventSource: (url) => new FakeEventSource(url),
    });
    const items: TraceEvent[] = [];
    monitoring.trace({ apis: ['GetObject'] }, { onItem: (e) => items.push(e) });
    await vi.waitFor(() => {
      expect(FakeEventSource.instances).toHaveLength(1);
    });
    const es = FakeEventSource.last();
    expect(new URL(es.url).searchParams.get('api')).toBe('GetObject');
    es.open();
    es.emit('trace', event);
    expect(items).toHaveLength(1);
  });

  it('never opens a stream closed before the implementation is loaded', async () => {
    const { monitoring } = createHttpServices({
      baseUrl: '/api/v1',
      eventSource: (url) => new FakeEventSource(url),
    });
    monitoring.logs({}, { onItem: () => undefined }).close();
    // A second stream proves that the module is loaded.
    monitoring.logs({ level: 'error' }, { onItem: () => undefined });
    await vi.waitFor(() => {
      expect(FakeEventSource.instances).toHaveLength(1);
    });
    expect(new URL(FakeEventSource.last().url).searchParams.get('level')).toBe('error');
  });
});

describe('HttpMonitoringService queries', () => {
  it('lists the series and queries one with its range and nodes', async () => {
    let query: Record<string, string> = {};
    server.use(
      http.get(api('/metrics'), () =>
        HttpResponse.json({
          items: [{ name: 's3_requests', description: 'R', unit: 'req/s', aggregation: 'sum' }],
        }),
      ),
      http.get(api('/metrics/query'), ({ request }) => {
        query = Object.fromEntries(new URL(request.url).searchParams);
        return HttpResponse.json({
          metric: { name: 's3_requests', description: 'R', unit: 'req/s', aggregation: 'sum' },
          start: '2026-10-15T09:00:00Z',
          end: '2026-10-15T10:00:00Z',
          stepSeconds: 10,
          nodes: [{ node: 'local', points: [{ t: 1, v: 2 }] }],
          cluster: [{ t: 1, v: 2 }],
        });
      }),
    );
    const { monitoring } = services();
    await expect(monitoring.metrics()).resolves.toHaveLength(1);
    const series = await monitoring.queryMetric({
      metric: 's3_requests',
      range: '6h',
      nodes: ['n1:9000'],
    });
    expect(series.cluster).toEqual([{ t: 1, v: 2 }]);
    expect(query).toEqual({ metric: 's3_requests', range: '6h', node: 'n1:9000' });
  });

  it('searches the audit log with the filters', async () => {
    let query: Record<string, string> = {};
    server.use(
      http.get(api('/audit'), ({ request }) => {
        query = Object.fromEntries(new URL(request.url).searchParams);
        return HttpResponse.json({ items: [] });
      }),
    );
    const { monitoring } = services();
    await expect(
      monitoring.audit({
        user: ' alice ',
        api: 'PutObject',
        bucket: '',
        statuses: ['403'],
        errorsOnly: true,
        since: '2026-10-15T09:00:00Z',
        limit: 500,
        type: 's3',
      }),
    ).resolves.toEqual([]);
    expect(query).toEqual({
      user: 'alice',
      api: 'PutObject',
      status: '403',
      errors: 'true',
      since: '2026-10-15T09:00:00Z',
      limit: '500',
      type: 's3',
    });
  });
});
