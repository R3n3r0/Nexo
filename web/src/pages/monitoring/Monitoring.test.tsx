import { act, configure, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import type { LogEntry, MetricSeries, Services, TraceEvent } from '@/services';
import { createHttpClient, type EventSourceLike } from '@/services/http';
import { createHttpMonitoringService } from '@/services/http/monitoring';
import { createMockServices, type MockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { monitoringRoutes } from '../monitoringRoutes';

// The page is a lazy chunk: under the load of the whole suite it takes
// longer to appear than the default waits.
configure({ asyncUtilTimeout: 5_000 });

const routes = monitoringRoutes();

/** A scriptable EventSource (jsdom has none): TD-U-12 drives the stream with it. */
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

  drop() {
    this.readyState = 0;
    this.onerror?.(new Event('error'));
  }

  static current(): FakeEventSource {
    const live = FakeEventSource.instances.filter((es) => !es.closed);
    const es = live.at(-1);
    if (es === undefined) throw new Error('no open EventSource');
    return es;
  }
}

function trace(n: number, extra: Partial<TraceEvent> = {}): TraceEvent {
  return {
    time: new Date(Date.UTC(2026, 9, 15, 10, 0, n)).toISOString(),
    node: 'local',
    type: 's3',
    api: 'PutObject',
    method: 'PUT',
    path: `/photos/file-${String(n)}.jpg`,
    status: 200,
    durationMs: 3 + n,
    inputBytes: 1024,
    outputBytes: 0,
    requestId: `REQ${String(n)}`,
    user: 'nexoadmin',
    bucket: 'photos',
    ...extra,
  };
}

function admin(options: Parameters<typeof createMockServices>[0] = {}): MockServices {
  return createMockServices({ initialAccessKey: 'nexoadmin', ...options });
}

/** The mock bundle with the live streams of the HTTP implementation over FakeEventSource. */
function withEventSource(): Services {
  const monitoring = createHttpMonitoringService(createHttpClient({ baseUrl: '/api/v1' }), {
    eventSource: (url) => new FakeEventSource(url),
  });
  return { ...admin(), monitoring };
}

/** A user allowed only the given admin actions. */
function limited(actions: string[]): MockServices {
  return createMockServices({
    accounts: { nexoadmin: 'nexoadmin', viewer: 'viewer-secret' },
    policies: [
      {
        name: 'monitoring-only',
        document: {
          Version: '2012-10-17',
          Statement: [{ Effect: 'Allow', Action: actions, Resource: ['arn:aws:s3:::*'] }],
        },
        builtIn: false,
        updatedAt: '2026-09-01T00:00:00Z',
      },
    ],
    users: [
      {
        accessKey: 'viewer',
        status: 'enabled',
        policyNames: ['monitoring-only'],
        groups: [],
        createdAt: '2026-09-01T00:00:00Z',
        updatedAt: '2026-09-01T00:00:00Z',
      },
    ],
    groups: [],
    initialAccessKey: 'viewer',
  });
}

function open(path: string, services: Services = admin()) {
  const result = renderRoutes(routes, { initialEntries: [path], services });
  return { ...result, user: userEvent.setup() };
}

beforeEach(() => {
  FakeEventSource.instances = [];
  // Recharts warns that jsdom gives its containers no size.
  const warn = console.warn.bind(console);
  vi.spyOn(console, 'warn').mockImplementation((...args: unknown[]) => {
    if (typeof args[0] === 'string' && args[0].startsWith('The width(0)')) return;
    warn(...args);
  });
  // jsdom has no layout: give the virtualized tables a size.
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(1200);
});

describe('monitoring page', { timeout: 30_000 }, () => {
  // covers: D-UX-02
  it('opens the first section allowed and offers only the allowed ones (D-UX-02)', async () => {
    const { router } = open('/monitoring', limited(['admin:ConsoleLog']));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/monitoring/logs');
    });
    const nav = await screen.findByRole('navigation', { name: 'Monitoring sections' });
    expect(
      within(nav)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['Live logs', 'Audit']);

    await act(() => router.navigate('/monitoring/trace'));
    expect(
      await screen.findByText('Your policies do not allow the section Live trace.'),
    ).toBeInTheDocument();
  });

  it('explains that no monitoring permission is granted', async () => {
    open('/monitoring', limited(['admin:ListUsers']));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Your policies grant none of the monitoring permissions',
    );
  });

  // covers: D-MON-01
  it('TD-U-11 draws the charts of the series with their latest values', async () => {
    const services = admin();
    const spy = vi.spyOn(services.monitoring, 'queryMetric');
    const { user, router } = open('/monitoring/metrics', services);
    for (const title of ['Requests', 'Errors', 'S3 latency', 'Bandwidth', 'Disk usage']) {
      expect(await screen.findByRole('region', { name: title })).toBeInTheDocument();
    }
    const latency = screen.getByRole('region', { name: 'S3 latency' });
    await waitFor(() => {
      expect(latency).toHaveTextContent(/Latest: p50 .* ms · p95 .* ms · p99 .* ms/);
    });
    // A single node: no internode chart, no comparison of the nodes.
    expect(screen.queryByRole('region', { name: 'Internode calls' })).not.toBeInTheDocument();
    expect(spy).toHaveBeenCalledWith({ metric: 's3_latency_p99', range: '1h' });

    await user.click(screen.getByRole('radio', { name: '6 hours' }));
    await waitFor(() => {
      expect(router.state.location.search).toBe('?range=6h');
    });
    await waitFor(() => {
      expect(spy).toHaveBeenCalledWith({ metric: 's3_requests', range: '6h' });
    });
  });

  // covers: D-MON-01
  it('TD-U-11 renders the charts of a range without sample', async () => {
    const services = admin();
    vi.spyOn(services.monitoring, 'queryMetric').mockImplementation((q): Promise<MetricSeries> =>
      Promise.resolve({
        metric: { name: q.metric, description: '', unit: 'req/s', aggregation: 'sum' },
        start: '2026-10-15T09:00:00Z',
        end: '2026-10-15T10:00:00Z',
        stepSeconds: 10,
        nodes: [{ node: 'local', points: [{ t: 1, v: null }] }],
        cluster: [{ t: 1, v: null }],
      }),
    );
    open('/monitoring/metrics', services);
    const requests = await screen.findByRole('region', { name: 'Requests' });
    expect(await within(requests).findByText(/No sample in this range yet/)).toBeInTheDocument();
  });

  it('compares the nodes of a cluster and scopes the charts to one node', async () => {
    const { sampleClusterInfo } = await import('@/services/mock');
    const services = admin({ info: sampleClusterInfo() });
    const spy = vi.spyOn(services.monitoring, 'queryMetric');
    const { user } = open('/monitoring/metrics', services);
    expect(await screen.findByRole('region', { name: 'Internode calls' })).toBeInTheDocument();
    expect(await screen.findByRole('region', { name: 'Compare the nodes' })).toBeInTheDocument();
    await user.click(screen.getByRole('textbox', { name: 'Scope' }));
    await user.click(await screen.findByRole('option', { name: 'nexo3:9000' }));
    await waitFor(() => {
      expect(spy).toHaveBeenCalledWith({
        metric: 's3_requests',
        range: '1h',
        nodes: ['nexo3:9000'],
      });
    });
    expect(screen.queryByRole('region', { name: 'Compare the nodes' })).not.toBeInTheDocument();
  });
});

// covers: D-MON-02
describe('live trace (TD-U-12)', { timeout: 30_000 }, () => {
  it('shows the requests as they come, pauses, resumes and bounds the buffer', async () => {
    const { user } = open('/monitoring/trace', withEventSource());
    await screen.findByRole('table', { name: 'Live requests' });
    const es = FakeEventSource.current();
    expect(new URL(es.url).pathname).toBe('/api/v1/trace');
    act(() => {
      es.open();
    });
    expect(await screen.findByRole('status', { name: 'Stream status: Live' })).toBeInTheDocument();

    act(() => {
      es.emit('trace', trace(1));
      es.emit('trace', trace(2, { status: 404, api: 'GetObject', error: 'Not Found' }));
    });
    const table = screen.getByRole('table', { name: 'Live requests' });
    expect(
      await within(table).findByText('/photos/file-2.jpg', { exact: false }),
    ).toBeInTheDocument();
    expect(within(table).getByText('404')).toBeInTheDocument();
    // Newest first.
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows[0]).toHaveTextContent('GetObject');

    await user.click(screen.getByRole('button', { name: 'Pause' }));
    act(() => {
      es.emit('trace', trace(3));
    });
    expect(await screen.findByText('1 new while paused')).toBeInTheDocument();
    expect(
      within(table).queryByText('/photos/file-3.jpg', { exact: false }),
    ).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Resume' }));
    expect(
      await within(table).findByText('/photos/file-3.jpg', { exact: false }),
    ).toBeInTheDocument();

    // The buffer keeps the last 100 requests.
    await user.click(screen.getByRole('textbox', { name: 'Lines kept' }));
    await user.click(await screen.findByRole('option', { name: 'Last 100' }));
    act(() => {
      for (let i = 10; i < 160; i++) es.emit('trace', trace(i));
    });
    expect(await screen.findByText('100 kept')).toBeInTheDocument();

    // Details of a request.
    await user.click(
      within(table).getAllByRole('button', { name: /^Details of PutObject at / })[0] as HTMLElement,
    );
    const details = await screen.findByRole('region', { name: 'Details' });
    expect(details).toHaveTextContent('REQ159');
  });

  it('reconnects after the connection broke and reports the dropped events', async () => {
    open('/monitoring/trace', withEventSource());
    await screen.findByRole('table', { name: 'Live requests' });
    const es = FakeEventSource.current();
    act(() => {
      es.open();
      es.drop();
    });
    expect(
      await screen.findByRole('status', { name: 'Stream status: Reconnecting' }),
    ).toBeInTheDocument();
    expect(screen.getByText('The connection was lost: reconnecting…')).toBeInTheDocument();
    act(() => {
      es.open();
      es.emit('dropped', { dropped: 42 });
      es.emit('trace', trace(1));
    });
    expect(await screen.findByRole('status', { name: 'Stream status: Live' })).toBeInTheDocument();
    expect(
      screen.getByText('The server dropped 42 events: the console did not keep up.'),
    ).toBeInTheDocument();
  });

  it('reopens the stream with the filters', async () => {
    const { user } = open('/monitoring/trace', withEventSource());
    await screen.findByRole('table', { name: 'Live requests' });
    await user.click(screen.getByRole('switch', { name: 'Errors only' }));
    await waitFor(() => {
      expect(new URL(FakeEventSource.current().url).searchParams.get('errors')).toBe('true');
    });
    const statuses = screen.getByRole('textbox', { name: 'Statuses' });
    await user.type(statuses, '7xx{Enter}');
    expect(await screen.findByText('Use status codes (404) or classes (5xx).')).toBeInTheDocument();
    await user.type(statuses, '5XX{Enter}');
    await waitFor(() => {
      expect(new URL(FakeEventSource.current().url).searchParams.get('status')).toBe('5xx');
    });
    await user.type(screen.getByRole('textbox', { name: 'Slower than' }), 'soon');
    expect(await screen.findByText('A duration such as 250ms, 2s or 1m.')).toBeInTheDocument();
    await user.clear(screen.getByRole('textbox', { name: 'Slower than' }));
    await user.type(screen.getByRole('textbox', { name: 'Slower than' }), '250ms');
    await waitFor(() => {
      expect(new URL(FakeEventSource.current().url).searchParams.get('threshold')).toBe('250ms');
    });
    // Only one stream is open at a time.
    expect(FakeEventSource.instances.filter((es) => !es.closed)).toHaveLength(1);
  });

  it('stops on a refusal of the server and restarts on demand', async () => {
    const services = limited(['admin:ConsoleLog', 'admin:ServerTrace']);
    const spy = vi.spyOn(services.monitoring, 'trace');
    spy.mockImplementationOnce((_filter, handlers) => {
      queueMicrotask(() => {
        handlers.onState?.('closed', undefined);
      });
      return { close: () => undefined };
    });
    const { user } = open('/monitoring/trace', services);
    expect(
      await screen.findByRole('status', { name: 'Stream status: Stopped' }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Restart' }));
    expect(await screen.findByRole('status', { name: 'Stream status: Live' })).toBeInTheDocument();
    expect(spy).toHaveBeenCalledTimes(2);
  });
});

// covers: D-MON-02
describe('live logs (TD-U-12)', { timeout: 30_000 }, () => {
  function log(n: number, level: LogEntry['level'], message: string): LogEntry {
    return {
      time: new Date(Date.UTC(2026, 9, 15, 10, 0, n)).toISOString(),
      node: 'local',
      level,
      message,
      attrs: { bucket: 'photos', n: String(n) },
    };
  }

  it('replays the recent entries, then streams and searches them', async () => {
    const services = admin();
    services.monitoring.emitLog(log(1, 'INFO', 'scanner: cycle done'));
    services.monitoring.emitLog(log(2, 'DEBUG', 'rpc: call'));
    const { user } = open('/monitoring/logs', services);
    const table = await screen.findByRole('table', { name: 'Live log entries' });
    // The default level (info) leaves out the debug entry.
    expect(await within(table).findByText('scanner: cycle done')).toBeInTheDocument();
    expect(within(table).queryByText('rpc: call')).not.toBeInTheDocument();
    act(() => {
      services.monitoring.emitLog(log(3, 'ERROR', 'heal: object failed'));
    });
    expect(await within(table).findByText('heal: object failed')).toBeInTheDocument();
    expect(within(table).getByText('ERROR')).toBeInTheDocument();

    await user.type(screen.getByRole('searchbox', { name: 'Search the lines' }), 'heal');
    await waitFor(() => {
      expect(within(table).queryByText('scanner: cycle done')).not.toBeInTheDocument();
    });
    expect(within(table).getByText('heal: object failed')).toBeInTheDocument();

    await user.click(screen.getByRole('radio', { name: 'Debug' }));
    await user.clear(screen.getByRole('searchbox', { name: 'Search the lines' }));
    expect(await within(table).findByText('rpc: call')).toBeInTheDocument();
  });
});

// covers: D-MON-03
describe('audit (D-MON-03)', { timeout: 30_000 }, () => {
  it('searches the records by user and outcome and shows their details', async () => {
    const { user } = open('/monitoring/audit');
    const table = await screen.findByRole('table', { name: 'Audit records' });
    expect(within(table).getAllByRole('row').length).toBeGreaterThan(2);

    await user.type(screen.getByRole('textbox', { name: 'User' }), 'alice');
    await user.click(screen.getByRole('textbox', { name: 'Outcome' }));
    await user.click(await screen.findByRole('option', { name: 'Failed' }));
    await user.click(screen.getByRole('button', { name: 'Search' }));
    expect(await screen.findByText('1 record, newest first')).toBeInTheDocument();
    const rows = within(screen.getByRole('table', { name: 'Audit records' })).getAllByRole('row');
    expect(rows[1]).toHaveTextContent('GetObject');
    expect(rows[1]).toHaveTextContent('403');

    await user.click(screen.getByRole('button', { name: 'Details of the record 17F09C' }));
    expect(await screen.findByText('curl/8.5')).toBeInTheDocument();
    expect(screen.getByText('Access Denied.')).toBeInTheDocument();
  });

  it('shows an empty search', async () => {
    const { user } = open('/monitoring/audit');
    await screen.findByRole('table', { name: 'Audit records' });
    await user.type(screen.getByRole('textbox', { name: 'Bucket' }), 'nothing-here');
    await user.click(screen.getByRole('button', { name: 'Search' }));
    expect(await screen.findByText('No audit record matches the search.')).toBeInTheDocument();
  });
});
