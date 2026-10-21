import { describe, expect, it } from 'vitest';

import {
  formatGoDuration,
  imagesMediaRule,
  isMediaImage,
  matchMediaRule,
  mediaRuleIssueMessage,
  normalizeMediaRule,
  parseGoDuration,
  validateMediaRule,
} from './mediaRules';
import type { MediaImageFormat, MediaRule } from '@/services';

describe('media rules', () => {
  it('validates and normalizes the automatic subtitles of a rule', () => {
    const rule: MediaRule = {
      id: 'talks',
      subtitles: { source: 'IT', languages: ['en', 'pt-br', 'en'] },
    };
    expect(validateMediaRule(rule)).toEqual([]);
    expect(normalizeMediaRule(rule).subtitles).toEqual({
      source: 'it',
      languages: ['en', 'pt-BR'],
    });
    expect(normalizeMediaRule({ id: 'x', subtitles: {} }).subtitles).toEqual({});
    const bad: MediaRule = { id: 'x', subtitles: { languages: ['en', '!!'] } };
    expect(validateMediaRule(bad)).toEqual(['subtitleLanguage']);
    expect(mediaRuleIssueMessage(bad, 'subtitleLanguage')).toBe('invalid language tag "!!"');
    const many: MediaRule = {
      id: 'x',
      subtitles: { languages: Array.from({ length: 21 }, (_, i) => `en-${String(100 + i)}`) },
    };
    expect(validateMediaRule(many)).toEqual(['subtitleLanguages']);
    expect(validateMediaRule({ id: 'img', images: {}, subtitles: {} })).toEqual([
      'imageVideoOutputs',
    ]);
  });

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

  it('validates an image rule as the server does, with its messages', () => {
    expect(validateMediaRule({ id: 'img', images: {} })).toEqual([]);
    expect(validateMediaRule(imagesMediaRule())).toEqual([]);
    const gif: MediaRule = { id: 'img', images: { formats: ['gif' as MediaImageFormat] } };
    expect(validateMediaRule(gif)).toEqual(['imageFormat']);
    expect(mediaRuleIssueMessage(gif, 'imageFormat')).toBe(
      'unknown image format "gif" (one of avif, webp, jpeg, png)',
    );
    const narrow: MediaRule = { id: 'img', images: { widths: [320, 8] } };
    expect(validateMediaRule(narrow)).toEqual(['imageWidth']);
    expect(mediaRuleIssueMessage(narrow, 'imageWidth')).toBe(
      'the image widths must be from 16 to 8192 pixels, not 8',
    );
    expect(validateMediaRule({ id: 'img', images: { widths: [8193] } })).toEqual(['imageWidth']);
    const many: MediaRule = {
      id: 'img',
      images: { widths: Array.from({ length: 17 }, (_, i) => 100 + i) },
    };
    expect(validateMediaRule(many)).toEqual(['imageWidths']);
    expect(mediaRuleIssueMessage(many, 'imageWidths')).toBe('at most 16 image widths');
    // Duplicates count once, as the server deduplicates before counting.
    expect(
      validateMediaRule({
        id: 'img',
        images: { widths: [...Array(16).keys(), 0].map((i) => 100 + i) },
      }),
    ).toEqual([]);
    const mixed: MediaRule = { id: 'img', poster: true, images: {} };
    expect(validateMediaRule(mixed)).toEqual(['imageVideoOutputs']);
    expect(mediaRuleIssueMessage(mixed, 'imageVideoOutputs')).toBe(
      'an image rule has no renditions, poster, thumbnails or subtitles',
    );
  });

  it('normalizes an image rule and matches images with its default suffixes', () => {
    expect(
      normalizeMediaRule({
        id: 'img',
        images: { formats: ['png', 'avif'], widths: [640, 160, 640] },
      }),
    ).toEqual({
      id: 'img',
      suffixes: [
        '.avif',
        '.bmp',
        '.gif',
        '.heic',
        '.heif',
        '.jpeg',
        '.jpg',
        '.png',
        '.tif',
        '.tiff',
        '.webp',
      ],
      images: { formats: ['avif', 'png'], widths: [160, 640] },
    });
    expect(normalizeMediaRule({ id: 'img', images: {} }).images).toEqual({
      formats: ['avif', 'webp', 'jpeg', 'png'],
      widths: [160, 320, 640, 1280, 1920, 2560],
    });
    const rules: MediaRule[] = [
      { id: 'v', poster: true },
      { id: 'img', images: {} },
    ];
    expect(matchMediaRule(rules, 'a.mp4')?.id).toBe('v');
    expect(matchMediaRule(rules, 'photos/Beach.JPG')?.id).toBe('img');
    expect(matchMediaRule(rules, 'doc.pdf')).toBeUndefined();
    expect(isMediaImage('a.heic')).toBe(true);
    expect(isMediaImage('photo', 'image/jpeg')).toBe(true);
    expect(isMediaImage('logo.svg', 'image/svg+xml')).toBe(false);
  });
});
