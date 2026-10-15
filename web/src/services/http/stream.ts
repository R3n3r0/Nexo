import { ApiError } from '../errors';
import type { LiveStream, StreamHandlers, StreamState } from '../interfaces';
import type { HttpClient, QueryParams } from './client';

/**
 * The part of the browser EventSource the streams use, so that the tests
 * substitute their own (jsdom has none).
 */
export interface EventSourceLike {
  /** 0 connecting, 1 open, 2 closed (EventSource.CONNECTING/OPEN/CLOSED). */
  readonly readyState: number;
  onopen: ((event: Event) => void) | null;
  onerror: ((event: Event) => void) | null;
  addEventListener(type: string, listener: (event: MessageEvent<string>) => void): void;
  close(): void;
}

/** Opens an EventSource on an absolute URL (sent with the session cookie). */
export type EventSourceFactory = (url: string) => EventSourceLike;

/** The browser EventSource; the console and the Admin API share their origin. */
export const browserEventSource: EventSourceFactory = (url) =>
  new EventSource(url, { withCredentials: true });

const eventSourceClosed = 2;

/** Delays of the reconnections after a refused or failed connection. */
export const reconnectDelaysMs: readonly number[] = [1000, 2000, 5000, 10_000, 30_000];

export interface EventStreamOptions<T> {
  client: HttpClient;
  eventSource: EventSourceFactory;
  path: string;
  query: QueryParams;
  /** The SSE event carrying the items ("trace", "log"). */
  event: string;
  handlers: StreamHandlers<T>;
  /** Timers (the tests drive them); defaults to the global ones. */
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (id: unknown) => void;
}

/**
 * A Server-Sent Events stream of the Admin API (GET /trace, GET /logs).
 *
 * The browser reconnects an EventSource by itself after a network failure
 * (the server sends "retry: 3000"); it gives up for good on an answer that
 * is not a stream (a 403, a 502 of a proxy, a 401 of an expired session).
 * Then the stream asks the same URL once with fetch to learn why: a client
 * error (4xx but 408 and 429) closes it with the problem of the server (a
 * 401 also signs the console out through the client), anything else
 * (5xx, network down, or a stream that works again) reopens it after a
 * growing delay. close() ends everything at once.
 */
export function openEventStream<T>(options: EventStreamOptions<T>): LiveStream {
  const { client, handlers } = options;
  const setTimer = options.setTimer ?? ((fn, ms) => setTimeout(fn, ms));
  const clearTimer =
    options.clearTimer ??
    ((id) => {
      clearTimeout(id as ReturnType<typeof setTimeout>);
    });
  const url = client.url(options.path, options.query);
  let source: EventSourceLike | undefined;
  let timer: unknown;
  let probe: AbortController | undefined;
  let attempts = 0;
  let closed = false;
  let state: StreamState | undefined;

  const setState = (next: StreamState, error?: ApiError) => {
    if (closed && next !== 'closed') return;
    if (state === next && error === undefined) return;
    state = next;
    handlers.onState?.(next, error);
  };

  const finish = (error?: ApiError) => {
    if (closed) return;
    setState('closed', error);
    closed = true;
    source?.close();
    source = undefined;
    probe?.abort();
    if (timer !== undefined) clearTimer(timer);
  };

  const schedule = () => {
    const delay = reconnectDelaysMs[Math.min(attempts, reconnectDelaysMs.length - 1)] ?? 30_000;
    attempts++;
    setState('reconnecting');
    timer = setTimer(() => {
      timer = undefined;
      connect();
    }, delay);
  };

  // The EventSource gave up: ask the server why with a plain request.
  const diagnose = async () => {
    const controller = new AbortController();
    probe = controller;
    try {
      await client.send(options.path, {
        query: options.query,
        headers: { Accept: 'text/event-stream' },
        signal: controller.signal,
      });
      // A stream again (the body is dropped): reopen it.
      controller.abort();
    } catch (cause) {
      if (closed) return;
      const error =
        cause instanceof ApiError ? cause : ApiError.network(cause ?? new Error('stream failed'));
      if (isFatal(error)) {
        finish(error);
        return;
      }
    } finally {
      if (probe === controller) probe = undefined;
    }
    if (!closed) schedule();
  };

  const connect = () => {
    if (closed) return;
    setState(attempts === 0 && state === undefined ? 'connecting' : 'reconnecting');
    const es = options.eventSource(url);
    source = es;
    es.onopen = () => {
      if (source !== es) return;
      attempts = 0;
      setState('open');
    };
    es.onerror = () => {
      if (source !== es || closed) return;
      if (es.readyState !== eventSourceClosed) {
        // The browser retries by itself.
        setState('reconnecting');
        return;
      }
      es.close();
      source = undefined;
      void diagnose();
    };
    es.addEventListener(options.event, (message) => {
      if (source !== es || closed) return;
      const item = parse<T>(message.data);
      if (item !== undefined) handlers.onItem(item);
    });
    es.addEventListener('dropped', (message) => {
      if (source !== es || closed) return;
      const body = parse<{ dropped?: unknown }>(message.data);
      if (body !== undefined && typeof body.dropped === 'number')
        handlers.onDropped?.(body.dropped);
    });
  };

  connect();
  return {
    close() {
      if (closed) return;
      closed = true;
      source?.close();
      source = undefined;
      probe?.abort();
      if (timer !== undefined) clearTimer(timer);
    },
  };
}

/** A refusal that a retry cannot fix: a client error but a timeout or a throttle. */
function isFatal(error: ApiError): boolean {
  return error.status >= 400 && error.status < 500 && error.status !== 408 && error.status !== 429;
}

function parse<T>(data: string): T | undefined {
  try {
    return JSON.parse(data) as T;
  } catch {
    return undefined;
  }
}
