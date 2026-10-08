import { describe, expect, it } from 'vitest';

import { formatBytes, formatCompact, formatDuration, formatNumber, splitDuration } from './format';

describe('formatBytes', () => {
  it('uses binary units with adaptive precision', () => {
    expect(formatBytes(0, 'en')).toBe('0 B');
    expect(formatBytes(512, 'en')).toBe('512 B');
    expect(formatBytes(1024, 'en')).toBe('1 KiB');
    expect(formatBytes(1536, 'en')).toBe('1.5 KiB');
    expect(formatBytes(194 * 1024 ** 3, 'en')).toBe('194 GiB');
    expect(formatBytes(2.5 * 1024 ** 4, 'en')).toBe('2.5 TiB');
  });

  it('returns a dash for invalid input', () => {
    expect(formatBytes(-1)).toBe('—');
    expect(formatBytes(Number.NaN)).toBe('—');
  });
});

describe('formatNumber / formatCompact', () => {
  it('groups digits and compacts large values', () => {
    expect(formatNumber(1284, 'en')).toBe('1,284');
    expect(formatCompact(1284, 'en')).toBe('1,284');
    expect(formatCompact(12_900, 'en')).toBe('12.9K');
    expect(formatCompact(4_200_000, 'en')).toBe('4.2M');
    expect(formatCompact(Number.POSITIVE_INFINITY)).toBe('—');
  });
});

describe('durations', () => {
  const label = (unit: string, count: number) => `${count}${unit[0] ?? ''}`;

  it('splits seconds into parts', () => {
    expect(splitDuration(3 * 86_400 + 4 * 3_600 + 12 * 60 + 5)).toEqual({
      days: 3,
      hours: 4,
      minutes: 12,
      seconds: 5,
    });
    expect(splitDuration(-5)).toEqual({ days: 0, hours: 0, minutes: 0, seconds: 0 });
  });

  it('keeps the two most significant units', () => {
    expect(formatDuration(3 * 86_400 + 4 * 3_600 + 12 * 60, label)).toBe('3d 4h');
    expect(formatDuration(3 * 86_400 + 12 * 60, label)).toBe('3d 0h');
    expect(formatDuration(4 * 3_600 + 12 * 60 + 5, label)).toBe('4h 12m');
    expect(formatDuration(65, label)).toBe('1m 5s');
    expect(formatDuration(5, label)).toBe('5s');
    expect(formatDuration(0, label)).toBe('0s');
  });
});
