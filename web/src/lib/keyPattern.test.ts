import { describe, expect, it } from 'vitest';

import type { KeyPattern } from '@/services';

import { compileKeyPattern, globToRegexSource, normalizeKey, re2ToJs } from './keyPattern';

function matches(pattern: KeyPattern, key: string): boolean {
  const result = compileKeyPattern(pattern);
  if (!result.ok) throw new Error(result.issue.message);
  return result.compiled.match(key).matched;
}

describe('globToRegexSource (internal/service/match/glob.go)', () => {
  it.each([
    ['*.txt', '[^/]*\\.txt'],
    ['docs/**/*.pdf', 'docs/(?:.*/)?[^/]*\\.pdf'],
    ['a**b', 'a.*b'],
    ['?', '[^/]'],
    ['[!a-c]', '[^/a-c]'],
    ['[]a]', '[\\]a]'],
    ['{img,video}/*', '(?:img|video)/[^/]*'],
    ['\\*', '\\*'],
  ])('translates %s', (glob, source) => {
    expect(globToRegexSource(glob)).toEqual({ source });
  });

  it.each([
    ['[abc', 1, 'unterminated character class'],
    ['{a,b', 1, 'unterminated alternation'],
    ['ab\\', 3, 'trailing backslash'],
    ['x[z-a]', 4, 'invalid character class range z-a'],
  ])('locates the error of %s', (glob, position, message) => {
    expect(globToRegexSource(glob)).toEqual({ issue: { position, message } });
  });
});

describe('compileKeyPattern', () => {
  it.each<[KeyPattern, string, boolean]>([
    [{ type: 'prefix', pattern: 'public/' }, 'public/a.txt', true],
    [{ type: 'prefix', pattern: 'public/' }, 'Public/a.txt', false],
    [{ type: 'prefix', pattern: 'public/', caseInsensitive: true }, 'Public/a.txt', true],
    [{ type: 'prefix', pattern: '' }, 'anything', true],
    [{ type: 'glob', pattern: '**/private/**' }, 'private/x', true],
    [{ type: 'glob', pattern: '**/private/**' }, 'a/b/private/c/d', true],
    [{ type: 'glob', pattern: '**/private/**' }, 'a/privateer/c', false],
    [{ type: 'glob', pattern: 'docs/*.pdf' }, 'docs/a/b.pdf', false],
    [{ type: 'glob', pattern: 'docs/**/*.pdf' }, 'docs/b.pdf', true],
    [{ type: 'glob', pattern: '{img,video}/*' }, 'video/a.mp4', true],
    [{ type: 'glob', pattern: 'file?.txt' }, 'file1.txt', true],
    [{ type: 'glob', pattern: 'file?.txt' }, 'file/.txt', false],
    [{ type: 'glob', pattern: 'a b+c.txt' }, 'a b+c.txt', true],
    [{ type: 'regex', pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png|webp)$' }, 'img/2026/a.JPG', false],
    [
      { type: 'regex', pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png|webp)$', caseInsensitive: true },
      'img/2026/a.JPG',
      true,
    ],
    [{ type: 'regex', pattern: '(?i)^IMG/' }, 'img/x', true],
    [{ type: 'regex', pattern: 'private' }, 'a/private/b', true],
    [{ type: 'regex', pattern: '^café/' }, 'café/menu', true],
    [{ type: 'regex', pattern: '^a\\-b$' }, 'a-b', true],
  ])('%j on %s → %s', (pattern, key, want) => {
    expect(matches(pattern, key)).toBe(want);
  });

  it('captures the named groups of RE2', () => {
    const result = compileKeyPattern({ type: 'regex', pattern: '^home/(?P<user>[^/]+)/' });
    expect(result.ok).toBe(true);
    if (result.ok) {
      expect(result.compiled.groups).toEqual(['user']);
      expect(result.compiled.match('home/mario/a.txt')).toEqual({
        matched: true,
        captures: { user: 'mario' },
      });
    }
  });

  it.each([
    ['(a)\\1', 4, 'backreferences are not supported (RE2)'],
    ['a(?=b)', 2, 'lookaround assertions are not supported (RE2)'],
    ['(?<!x)y', 1, 'lookaround assertions are not supported (RE2)'],
  ])('refuses %s like RE2', (pattern, position, message) => {
    const result = compileKeyPattern({ type: 'regex', pattern });
    expect(result).toEqual({ ok: false, issue: { position, message } });
  });

  it('reports the syntax errors and the patterns that are too long', () => {
    const syntax = compileKeyPattern({ type: 'regex', pattern: '(abc' });
    expect(syntax.ok).toBe(false);
    const long = compileKeyPattern({ type: 'regex', pattern: 'a'.repeat(1025) });
    expect(long).toMatchObject({ ok: false, issue: { position: 1025 } });
  });

  it('marks the RE2 syntax the browser cannot run as unsupported', () => {
    expect(re2ToJs('[[:alpha:]]')).toMatchObject({ issue: { unsupported: true } });
    expect(re2ToJs('(?U)a+')).toMatchObject({ issue: { unsupported: true } });
  });

  it('runs a pathological expression in reasonable time', () => {
    // RE2 is linear; the preview must not hang on this classic case.
    const result = compileKeyPattern({ type: 'regex', pattern: '^(a|b)*c$' });
    expect(result.ok).toBe(true);
    if (result.ok) {
      const started = performance.now();
      expect(result.compiled.match(`${'a'.repeat(1024)}`).matched).toBe(false);
      expect(performance.now() - started).toBeLessThan(500);
    }
  });
});

describe('normalizeKey (internal/service/match/normalize.go)', () => {
  it.each([
    ['a//b', 'a/b'],
    ['./a/b', 'a/b'],
    ['x/../a/b', 'a/b'],
    ['../../a', 'a'],
    ['a/b/', 'a/b/'],
    ['/a/./b', '/a/b'],
    ['a/b/..', 'a/'],
    ['plain/key.txt', 'plain/key.txt'],
    ['café', 'café'],
  ])('%s → %s', (key, want) => {
    expect(normalizeKey(key)).toBe(want);
  });
});
