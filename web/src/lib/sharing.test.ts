import { describe, expect, it } from 'vitest';

import { checkConfigValue } from './config';
import {
  checkAccessKey,
  checkSecretKey,
  generateAccessKey,
  generateSecretKey,
} from './credentials';
import { checkExpiryDate, maxExpirySeconds } from './expiry';
import { postSnippets, putSnippets, shellQuote } from './uploadSnippets';

describe('checkExpiryDate', () => {
  const now = new Date('2026-10-08T12:00:00Z');
  it.each([
    [null, 'required'],
    [new Date('2026-10-08T11:59:59Z'), 'past'],
    [new Date('2026-10-08T12:00:00Z'), 'past'],
    [new Date('2026-10-08T13:00:00Z'), undefined],
    [new Date(now.getTime() + maxExpirySeconds * 1000), undefined],
    [new Date(now.getTime() + maxExpirySeconds * 1000 + 1000), 'tooFar'],
  ])('%s → %s', (date, problem) => {
    expect(checkExpiryDate(date, now)).toBe(problem);
  });
});

describe('credentials', () => {
  it('generates keys that pass the server rules', () => {
    for (let i = 0; i < 20; i += 1) {
      expect(checkAccessKey(generateAccessKey())).toBeUndefined();
      expect(checkSecretKey(generateSecretKey())).toBeUndefined();
    }
    expect(generateAccessKey()).not.toBe(generateAccessKey());
  });

  it.each([
    ['', 'required'],
    ['ab', 'length'],
    ['a'.repeat(21), 'length'],
    ['bad key', 'characters'],
    ['ok.user@x+1', undefined],
  ])('access key %j → %s', (value, problem) => {
    expect(checkAccessKey(value)).toBe(problem);
  });

  it.each([
    ['', 'required'],
    ['short', 'length'],
    ['x'.repeat(41), 'length'],
    ['tab\tinside', 'characters'],
    ['good-secret', undefined],
  ])('secret key %j → %s', (value, problem) => {
    expect(checkSecretKey(value)).toBe(problem);
  });
});

describe('checkConfigValue', () => {
  const key = (type: 'bool' | 'int' | 'duration' | 'url' | 'enum' | 'list', values?: string[]) => ({
    name: 'k',
    type,
    description: '',
    sensitive: false,
    applied: false,
    ...(values ? { values } : {}),
  });
  it.each([
    [key('bool'), 'on', undefined],
    [key('bool'), 'yes', 'bool'],
    [key('int'), '42', undefined],
    [key('int'), '-1', 'int'],
    [key('duration'), '1h30m', undefined],
    [key('duration'), 'soon', 'duration'],
    [key('url'), 'https://hooks.example/x', undefined],
    [key('url'), 'ftp://x', 'url'],
    [key('enum', ['a', 'b']), 'b', undefined],
    [key('enum', ['a', 'b']), 'c', 'enum'],
    [key('list'), 'a,b', undefined],
    [key('list'), 'a,,b', 'list'],
    [key('int'), '', undefined],
  ])('%j %j → %s', (k, value, problem) => {
    expect(checkConfigValue(k, value)).toBe(problem);
  });
});

describe('upload snippets', () => {
  it('quotes values for the shell', () => {
    expect(shellQuote("it's")).toBe(`'it'\\''s'`);
  });

  it('builds curl and fetch snippets of a PUT URL with its signed headers', () => {
    const snippets = putSnippets({
      url: 'http://s3/b/k?X-Amz-Signature=1',
      method: 'PUT',
      expiresAt: '2026-10-08T13:00:00Z',
      headers: { 'Content-Type': 'image/png' },
    });
    expect(snippets.curl).toContain(`-H 'Content-Type: image/png'`);
    expect(snippets.curl).toContain(`'http://s3/b/k?X-Amz-Signature=1'`);
    expect(snippets.javascript).toContain('"Content-Type": "image/png"');
    expect(snippets.html).toBeUndefined();
  });

  it('builds an HTML form with escaped fields and the file last', () => {
    const snippets = postSnippets({
      url: 'http://s3/b',
      expiresAt: '2026-10-08T13:00:00Z',
      conditions: [],
      fields: { key: 'up/${filename}', policy: 'a"b<c' },
    });
    expect(snippets.html).toContain('value="a&quot;b&lt;c"');
    expect(snippets.html?.indexOf('name="file"')).toBeGreaterThan(
      snippets.html?.indexOf('name="policy"') ?? Infinity,
    );
    expect(snippets.curl).toContain(`--form-string 'key=up/\${filename}'`);
    expect(snippets.curl.trim().endsWith(`'http://s3/b'`)).toBe(true);
    expect(snippets.javascript).toContain(`form.append("policy", "a\\"b<c");`);
  });

  it('sends the policy fields of the curl snippet verbatim, never as local files', () => {
    const snippets = postSnippets({
      url: 'http://s3/b',
      expiresAt: '2026-10-08T13:00:00Z',
      conditions: [],
      fields: { key: '@inbox/${filename}', 'x-amz-meta-note': '<notes.txt', policy: 'p' },
    });
    // With -F, curl reads the file named after "@" or "<".
    expect(snippets.curl).toContain(`--form-string 'key=@inbox/\${filename}'`);
    expect(snippets.curl).toContain(`--form-string 'x-amz-meta-note=<notes.txt'`);
    const fileFields = snippets.curl.split('\n').filter((line) => line.trim().startsWith('-F '));
    expect(fileFields).toEqual(['  -F file=@./file \\']);
  });
});
