import { describe, expect, it } from 'vitest';

import {
  canonicalLanguage,
  cueProblem,
  formatCueTime,
  formatSRT,
  formatWebVTT,
  languageName,
  parseCueTime,
  parseSubtitles,
} from './subtitles';

describe('subtitles', () => {
  it('canonicalizes language tags', () => {
    expect(canonicalLanguage('pt-br')).toBe('pt-BR');
    expect(canonicalLanguage(' it ')).toBe('it');
    expect(canonicalLanguage('zh-hant-tw')).toBe('zh-Hant-TW');
    expect(canonicalLanguage('')).toBeUndefined();
    expect(canonicalLanguage('it_IT/..')).toBeUndefined();
    expect(canonicalLanguage('x')).toBeUndefined();
  });

  it('names languages in their own language', () => {
    expect(languageName('it')).toBe('Italiano');
    expect(languageName('en')).toBe('English');
  });

  it('formats and parses times', () => {
    expect(formatCueTime(3723.5)).toBe('01:02:03.500');
    expect(formatCueTime(1.25, ',')).toBe('00:00:01,250');
    expect(parseCueTime('01:02:03.500')).toBe(3723.5);
    expect(parseCueTime('02:03,040')).toBeCloseTo(123.04);
    expect(parseCueTime('2:3.4')).toBeUndefined();
    expect(parseCueTime('00:61.000')).toBeUndefined();
  });

  it('parses WebVTT and SubRip, and formats both', () => {
    const vtt =
      '﻿WEBVTT\n\nNOTE x\n\nid\n00:02.000 --> 00:03.000 line:0\nTwo\n\n00:00:00.500 --> 00:00:01.000\nOne\nline\n';
    const cues = parseSubtitles(vtt);
    expect(cues).toEqual([
      { start: 0.5, end: 1, text: 'One\nline' },
      { start: 2, end: 3, text: 'Two', settings: 'line:0' },
    ]);
    expect(parseSubtitles(formatWebVTT(cues))).toEqual(cues);
    expect(formatSRT(cues)).toBe(
      '1\n00:00:00,500 --> 00:00:01,000\nOne\nline\n\n2\n00:00:02,000 --> 00:00:03,000\nTwo\n',
    );
    expect(parseSubtitles('1\r\n00:00:01,000 --> 00:00:02,000\r\nCiao\r\n')).toEqual([
      { start: 1, end: 2, text: 'Ciao' },
    ]);
    expect(() => parseSubtitles('WEBVTT\n\n00:01 --> 00:02.000\nx\n')).toThrow();
  });

  it('reports invalid cues', () => {
    expect(cueProblem({ start: 1, end: 2, text: 'ok' })).toBeUndefined();
    expect(cueProblem({ start: 2, end: 2, text: 'x' })).toBe('order');
    expect(cueProblem({ start: 0, end: 1, text: '  ' })).toBe('text');
    expect(cueProblem({ start: 0, end: 1, text: 'a'.repeat(3000) })).toBe('long');
  });
});
