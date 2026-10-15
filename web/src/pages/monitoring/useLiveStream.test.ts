import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { LiveStream, StreamHandlers } from '@/services';
import { ApiError } from '@/services/errors';

import { useLiveStream } from './useLiveStream';

/** The streams opened by the hook, with their handlers (newest last). */
interface Opened {
  filter: { q: string };
  handlers: StreamHandlers<number>;
  closed: boolean;
}

function harness() {
  const opened: Opened[] = [];
  const open = vi.fn((filter: { q: string }, handlers: StreamHandlers<number>): LiveStream => {
    const entry: Opened = { filter, handlers, closed: false };
    opened.push(entry);
    return {
      close() {
        entry.closed = true;
      },
    };
  });
  const current = () => {
    const s = opened.at(-1);
    if (s === undefined) throw new Error('no stream opened');
    return s;
  };
  const emit = (...items: number[]) => {
    act(() => {
      for (const item of items) current().handlers.onItem(item);
    });
  };
  const flush = () => {
    act(() => {
      vi.advanceTimersByTime(200);
    });
  };
  return { opened, open, current, emit, flush };
}

const values = (items: readonly { value: number }[]) => items.map((i) => i.value);

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

// covers: D-MON-02
describe('useLiveStream (TD-U-12)', () => {
  it('batches the items, newest first, within the limit of the buffer', () => {
    const h = harness();
    const { result, rerender } = renderHook(
      ({ limit }) => useLiveStream(h.open, { q: 'a' }, { limit, flushMs: 100 }),
      { initialProps: { limit: 3 } },
    );
    expect(h.open).toHaveBeenCalledTimes(1);
    act(() => {
      h.current().handlers.onState?.('open');
    });
    expect(result.current.state).toBe('open');

    h.emit(1, 2);
    expect(result.current.items).toHaveLength(0);
    h.flush();
    expect(values(result.current.items)).toEqual([2, 1]);

    h.emit(3, 4, 5);
    h.flush();
    expect(values(result.current.items)).toEqual([5, 4, 3]);
    // Sequence numbers are unique keys.
    expect(new Set(result.current.items.map((i) => i.seq)).size).toBe(3);

    // A smaller buffer trims at once.
    rerender({ limit: 2 });
    expect(values(result.current.items)).toEqual([5, 4]);
  });

  it('holds the items while paused, bounded, and shows them on resume', () => {
    const h = harness();
    const { result } = renderHook(() => useLiveStream(h.open, { q: 'a' }, { limit: 3 }));
    h.emit(1);
    h.flush();
    act(() => {
      result.current.pause();
    });
    h.emit(2, 3, 4, 5);
    h.flush();
    expect(result.current.paused).toBe(true);
    expect(values(result.current.items)).toEqual([1]);
    expect(result.current.pending).toBe(4);

    act(() => {
      result.current.resume();
    });
    expect(result.current.paused).toBe(false);
    expect(result.current.pending).toBe(0);
    // Only the last three held items survive the bound of the buffer.
    expect(values(result.current.items)).toEqual([5, 4, 3]);

    act(() => {
      result.current.clear();
    });
    expect(result.current.items).toHaveLength(0);
  });

  it('reopens the stream for a new filter and starts a new list', () => {
    const h = harness();
    const { result, rerender } = renderHook(
      ({ q }) => useLiveStream(h.open, { q }, { limit: 10 }),
      { initialProps: { q: 'a' } },
    );
    h.emit(1);
    h.flush();
    const first = h.current();
    // An equal filter keeps the stream.
    rerender({ q: 'a' });
    expect(h.open).toHaveBeenCalledTimes(1);

    rerender({ q: 'b' });
    expect(first.closed).toBe(true);
    expect(h.open).toHaveBeenCalledTimes(2);
    expect(h.current().filter).toEqual({ q: 'b' });
    expect(result.current.items).toHaveLength(0);
  });

  it('reports the drops, the closing error and restarts on demand', () => {
    const h = harness();
    const { result, unmount } = renderHook(() => useLiveStream(h.open, { q: 'a' }, { limit: 10 }));
    act(() => {
      h.current().handlers.onState?.('open');
      h.current().handlers.onDropped?.(7);
    });
    expect(result.current.dropped).toBe(7);

    act(() => {
      h.current().handlers.onState?.('reconnecting');
    });
    expect(result.current.state).toBe('reconnecting');
    // A new connection resets the counter of the server.
    act(() => {
      h.current().handlers.onState?.('open');
    });
    expect(result.current.dropped).toBe(0);

    const denied = new ApiError({ status: 403, code: 'access_denied', title: 'Forbidden' });
    act(() => {
      h.current().handlers.onState?.('closed', denied);
    });
    expect(result.current.state).toBe('closed');
    expect(result.current.error).toBe(denied);

    act(() => {
      result.current.restart();
    });
    expect(h.open).toHaveBeenCalledTimes(2);
    expect(result.current.error).toBeUndefined();

    unmount();
    expect(h.current().closed).toBe(true);
  });

  it('keeps the stream closed while disabled', () => {
    const h = harness();
    renderHook(() => useLiveStream(h.open, { q: 'a' }, { limit: 10, enabled: false }));
    expect(h.open).not.toHaveBeenCalled();
  });
});
