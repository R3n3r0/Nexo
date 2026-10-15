import { describe, expect, it } from 'vitest';

import type { TraceEvent } from '@/services';

import {
  certificateState,
  chartRows,
  formatMetricValue,
  formatMs,
  isStatusFilter,
  lastValue,
  levelRank,
  parseGoDuration,
  refreshMs,
  statusMatches,
  traceMatches,
} from './monitoring';

describe('parseGoDuration', () => {
  it.each([
    ['500ms', 500],
    ['2s', 2000],
    ['1.5s', 1500],
    ['1m30s', 90_000],
    ['1h', 3_600_000],
    ['250us', 0.25],
    ['0', 0],
    [' 10s ', 10_000],
  ])('%s is %d ms', (text, ms) => {
    expect(parseGoDuration(text)).toBeCloseTo(ms);
  });

  it.each(['', 'soon', '10', '5 s', 's10', '1d', '-1s', '1s2'])('rejects %j', (text) => {
    expect(parseGoDuration(text)).toBeUndefined();
  });
});

describe('statuses', () => {
  it.each([
    [404, ['404'], true],
    [404, ['4xx'], true],
    [404, ['5xx', '403'], false],
    [503, ['5XX'], true],
    [200, [], true],
    [200, undefined, true],
  ] as const)('%d against %j is %s', (status, filter, want) => {
    expect(statusMatches(status, filter)).toBe(want);
  });

  it.each([
    ['404', true],
    ['5xx', true],
    ['5XX', true],
    ['7xx', false],
    ['99', false],
    ['600', false],
    ['abc', false],
  ])('%s is a status filter: %s', (value, want) => {
    expect(isStatusFilter(value)).toBe(want);
  });
});

describe('traceMatches', () => {
  const event: TraceEvent = {
    time: '2026-10-15T10:00:00Z',
    node: 'n1:9000',
    type: 's3',
    api: 'PutObject',
    method: 'PUT',
    path: '/b/k',
    status: 503,
    durationMs: 750,
    inputBytes: 0,
    outputBytes: 0,
    bucket: 'b',
  };

  it.each([
    [{}, true],
    [{ types: ['admin' as const] }, false],
    [{ nodes: ['n1:9000'] }, true],
    [{ nodes: ['n2:9000'] }, false],
    [{ apis: ['putobject'] }, true],
    [{ buckets: ['other'] }, false],
    [{ statuses: ['5xx'] }, true],
    [{ errorsOnly: true }, true],
    [{ threshold: '500ms' }, true],
    [{ threshold: '1s' }, false],
  ])('%j: %s', (filter, want) => {
    expect(traceMatches(event, filter)).toBe(want);
  });

  it('leaves out the internode calls unless asked', () => {
    const rpc = { ...event, type: 'rpc' as const };
    expect(traceMatches(rpc, {})).toBe(false);
    expect(traceMatches(rpc, { types: ['rpc'] })).toBe(true);
  });
});

describe('formatting', () => {
  it.each([
    [0.42, '0.42 ms'],
    [12.34, '12.3 ms'],
    [250, '250 ms'],
    [1500, '1.5 s'],
  ])('formatMs(%d) is %s', (ms, want) => {
    expect(formatMs(ms, 'en')).toBe(want);
  });

  it.each([
    [null, 'req/s', '—'],
    [1.234, 'req/s', '1.23 req/s'],
    [1536, 'bytes', '1.5 KiB'],
    [2048, 'bytes/s', '2 KiB/s'],
    [0.0123, 'seconds', '12.3 ms'],
    [12_840, 'objects', '12.8K objects'],
    [0.5, 'cores', '0.5'],
    [1, 'boolean', '1'],
  ] as const)('formatMetricValue(%s, %s) is %s', (value, unit, want) => {
    expect(formatMetricValue(value, unit, 'en')).toBe(want);
  });

  it('ranks the levels', () => {
    expect(levelRank('debug')).toBeLessThan(levelRank('INFO'));
    expect(levelRank('ERROR')).toBe(3);
    expect(levelRank('unknown')).toBe(1);
  });

  it('turns the refresh choices into intervals', () => {
    expect(refreshMs('off')).toBe(false);
    expect(refreshMs('10s')).toBe(10_000);
    expect(refreshMs('1m')).toBe(60_000);
  });
});

describe('chartRows', () => {
  it('merges the series by time, missing points as gaps', () => {
    const rows = chartRows([
      {
        key: 'a',
        points: [
          { t: 10, v: 1 },
          { t: 20, v: null },
        ],
      },
      {
        key: 'b',
        points: [
          { t: 20, v: 2 },
          { t: 30, v: 3 },
        ],
      },
    ]);
    expect(rows).toEqual([
      { t: 10, a: 1, b: null },
      { t: 20, a: null, b: 2 },
      { t: 30, a: null, b: 3 },
    ]);
    expect(chartRows([])).toEqual([]);
  });

  it('finds the last value of a series', () => {
    expect(
      lastValue([
        { t: 1, v: 4 },
        { t: 2, v: null },
      ]),
    ).toBe(4);
    expect(lastValue([{ t: 1, v: null }])).toBeNull();
  });
});

describe('certificateState', () => {
  const now = new Date('2026-10-15T00:00:00Z');
  it.each([
    ['2026-01-01T00:00:00Z', '2027-01-01T00:00:00Z', 'valid'],
    ['2026-01-01T00:00:00Z', '2026-10-25T00:00:00Z', 'expiring'],
    ['2026-01-01T00:00:00Z', '2026-10-01T00:00:00Z', 'expired'],
    ['2026-11-01T00:00:00Z', '2027-01-01T00:00:00Z', 'notYetValid'],
  ])('%s → %s is %s', (notBefore, notAfter, want) => {
    expect(certificateState({ notBefore, notAfter }, now).state).toBe(want);
  });

  it('counts the days left', () => {
    expect(
      certificateState({ notBefore: '2026-01-01T00:00:00Z', notAfter: '2026-10-25T00:00:00Z' }, now)
        .days,
    ).toBe(10);
  });
});
