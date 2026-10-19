import { describe, expect, it } from 'vitest';

import {
  formatGoDuration,
  matchMediaRule,
  normalizeMediaRule,
  parseGoDuration,
  validateMediaRule,
} from './mediaRules';

describe('media rules', () => {
  it('parses and formats Go durations', () => {
    expect(parseGoDuration('10s')).toBe(10);
    expect(parseGoDuration('1m30s')).toBe(90);
    expect(parseGoDuration('1.5m')).toBe(90);
    expect(parseGoDuration('500ms')).toBe(0.5);
    expect(parseGoDuration('10')).toBeUndefined();
    expect(parseGoDuration('ten')).toBeUndefined();
    expect(parseGoDuration('')).toBeUndefined();
    expect(formatGoDuration(10)).toBe('10s');
    expect(formatGoDuration(90)).toBe('1m30s');
    expect(formatGoDuration(600)).toBe('10m0s');
  });

  it('validates a rule as the server does', () => {
    expect(validateMediaRule({ id: 'videos', renditions: ['720p'] })).toEqual([]);
    expect(validateMediaRule({ id: 'a b', poster: true })).toEqual(['id']);
    expect(validateMediaRule({ id: 'a', poster: true }, ['a'])).toEqual(['idDuplicate']);
    expect(validateMediaRule({ id: 'a', prefix: '.nexo-media/v/', poster: true })).toEqual([
      'prefixReserved',
    ]);
    expect(validateMediaRule({ id: 'a', suffixes: [' '], poster: true })).toEqual(['suffix']);
    expect(validateMediaRule({ id: 'a', thumbnails: { interval: '500ms' } })).toEqual(['interval']);
    expect(validateMediaRule({ id: 'a', thumbnails: { interval: '11m' } })).toEqual(['interval']);
    expect(validateMediaRule({ id: 'a', thumbnails: {} })).toEqual([]);
    expect(validateMediaRule({ id: 'a' })).toEqual(['nothing']);
  });

  it('normalizes a rule and matches keys with the first rule', () => {
    const rule = normalizeMediaRule({
      id: 'v',
      prefix: '',
      renditions: ['360p', '1080p'],
      thumbnails: {},
    });
    expect(rule).toEqual({
      id: 'v',
      suffixes: ['.m4v', '.mov', '.mp4'],
      renditions: ['1080p', '360p'],
      thumbnails: { interval: '10s' },
    });
    const rules = [{ id: 'clips', prefix: 'clips/', suffixes: ['.webm'] }, { id: 'all' }];
    expect(matchMediaRule(rules, 'clips/a.webm')?.id).toBe('clips');
    expect(matchMediaRule(rules, 'clips/a.MP4')?.id).toBe('all');
    expect(matchMediaRule(rules, 'doc.pdf')).toBeUndefined();
    expect(matchMediaRule(rules, '.nexo-media/x.mp4')).toBeUndefined();
  });
});
