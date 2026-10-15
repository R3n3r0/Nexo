import { useCallback, useEffect, useRef, useState } from 'react';

import type { ApiError, LiveStream, StreamHandlers, StreamState } from '@/services';

/** An item of a live view with its sequence number (a stable key). */
export interface LiveEntry<T> {
  seq: number;
  value: T;
}

/** The live view of a stream: the items kept and the controls (TD-U-12). */
export interface LiveStreamView<T> {
  /** The items kept, newest first, at most limit. */
  items: readonly LiveEntry<T>[];
  state: StreamState;
  /** Why the server closed the stream (state closed). */
  error: ApiError | undefined;
  /** Events the server dropped on the current connection (a slow client). */
  dropped: number;
  paused: boolean;
  /** Items received while paused, shown on resume (at most limit). */
  pending: number;
  pause: () => void;
  resume: () => void;
  /** Forgets the items kept and held. */
  clear: () => void;
  /** Opens the stream again (after it was closed by the server). */
  restart: () => void;
}

export interface LiveStreamOptions {
  /** The items kept (the oldest are dropped first). */
  limit: number;
  /** False keeps the stream closed. */
  enabled?: boolean;
  /** Items are rendered in batches at most this often (ms). */
  flushMs?: number;
}

/**
 * Subscribes to a live stream (trace, logs) for a filter and keeps its
 * last items. The rendering is batched (a busy server sends hundreds of
 * events per second); pause() keeps receiving but holds the items (still
 * bounded by limit) until resume(). A change of the filter reopens the
 * stream and starts a new list; the stream itself reconnects after a
 * network failure, restart() reopens one the server closed.
 */
export function useLiveStream<T, F>(
  open: (filter: F, handlers: StreamHandlers<T>) => LiveStream,
  filter: F,
  { limit, enabled = true, flushMs = 150 }: LiveStreamOptions,
): LiveStreamView<T> {
  const [items, setItems] = useState<readonly LiveEntry<T>[]>([]);
  const [state, setState] = useState<StreamState>('connecting');
  const [error, setError] = useState<ApiError | undefined>(undefined);
  const [dropped, setDropped] = useState(0);
  const [paused, setPaused] = useState(false);
  const [pending, setPending] = useState(0);
  const [generation, setGeneration] = useState(0);

  const limitRef = useRef(limit);
  const pausedRef = useRef(false);
  const queue = useRef<T[]>([]);
  const held = useRef<T[]>([]);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const seq = useRef(0);
  const openRef = useRef(open);
  openRef.current = open;

  const flush = useCallback(() => {
    timer.current = undefined;
    if (queue.current.length === 0) return;
    const fresh = queue.current.map((value) => ({ seq: ++seq.current, value })).reverse();
    queue.current = [];
    setItems((prev) => [...fresh, ...prev].slice(0, limitRef.current));
  }, []);

  const schedule = useCallback(() => {
    timer.current ??= setTimeout(flush, flushMs);
  }, [flush, flushMs]);

  // A new limit trims the items kept at once.
  useEffect(() => {
    limitRef.current = limit;
    setItems((prev) => (prev.length > limit ? prev.slice(0, limit) : prev));
    if (held.current.length > limit) held.current = held.current.slice(-limit);
  }, [limit]);

  const filterKey = JSON.stringify(filter);
  useEffect(() => {
    if (!enabled) return undefined;
    setItems([]);
    setError(undefined);
    setDropped(0);
    queue.current = [];
    held.current = [];
    setPending(0);
    const parsed = JSON.parse(filterKey) as F;
    const stream = openRef.current(parsed, {
      onItem(item) {
        if (pausedRef.current) {
          held.current.push(item);
          if (held.current.length > limitRef.current) held.current.shift();
          setPending((n) => n + 1);
          return;
        }
        queue.current.push(item);
        if (queue.current.length > limitRef.current) queue.current.shift();
        schedule();
      },
      onDropped(total) {
        setDropped(total);
      },
      onState(next, cause) {
        setState(next);
        if (next === 'open') setDropped(0);
        setError(next === 'closed' ? cause : undefined);
      },
    });
    return () => {
      stream.close();
      if (timer.current !== undefined) clearTimeout(timer.current);
      timer.current = undefined;
    };
  }, [filterKey, enabled, generation, schedule]);

  const pause = useCallback(() => {
    pausedRef.current = true;
    setPaused(true);
  }, []);

  const resume = useCallback(() => {
    pausedRef.current = false;
    setPaused(false);
    queue.current.push(...held.current);
    held.current = [];
    setPending(0);
    flush();
  }, [flush]);

  const clear = useCallback(() => {
    queue.current = [];
    held.current = [];
    setPending(0);
    setItems([]);
  }, []);

  const restart = useCallback(() => {
    setGeneration((g) => g + 1);
  }, []);

  return { items, state, error, dropped, paused, pending, pause, resume, clear, restart };
}
