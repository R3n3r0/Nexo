import { describe, expect, it } from 'vitest';

import { bucketNameIssue } from './bucketName';
import {
  baseName,
  browsePath,
  decodeKeyPath,
  encodeKeyPath,
  folderNameIssue,
  parentPrefix,
  prefixFromPathname,
  prefixSegments,
} from './objectPath';
import { fromBytes, toBytes } from './units';

describe('object keys in routes (TD-U-05)', () => {
  it.each([
    'plain/key.txt',
    'with space/file name.pdf',
    'hash#/100% done.txt',
    'città/naïve 日本語.png',
    'query?a=b&c/plus+sign',
    'trailing/',
  ])('round-trips %s through the path', (key) => {
    expect(decodeKeyPath(encodeKeyPath(key))).toBe(key);
    const path = browsePath('media', key);
    expect(path).not.toMatch(/[ #?]/);
    expect(prefixFromPathname(path, 'media')).toBe(key);
  });

  it('encodes every segment but keeps the separators', () => {
    expect(browsePath('media', 'a b/c#d/')).toBe('/buckets/media/browse/a%20b/c%23d/');
    expect(browsePath('media')).toBe('/buckets/media/browse/');
  });

  it('reads the root of the bucket', () => {
    expect(prefixFromPathname('/buckets/media/browse', 'media')).toBe('');
    expect(prefixFromPathname('/buckets/media/browse/', 'media')).toBe('');
    expect(prefixFromPathname('/buckets/media/settings', 'media')).toBe('');
  });

  it('keeps malformed escapes verbatim', () => {
    expect(decodeKeyPath('a%zz/b')).toBe('a%zz/b');
  });

  it('splits a prefix into breadcrumb segments', () => {
    expect(prefixSegments('')).toEqual([]);
    expect(prefixSegments('a/b c/')).toEqual([
      { name: 'a', prefix: 'a/' },
      { name: 'b c', prefix: 'a/b c/' },
    ]);
  });

  it('computes names and parents', () => {
    expect(baseName('a/b/c.txt')).toBe('c.txt');
    expect(baseName('a/b/')).toBe('b/');
    expect(baseName('top.txt')).toBe('top.txt');
    expect(parentPrefix('a/b/c.txt')).toBe('a/b/');
    expect(parentPrefix('a/')).toBe('');
  });

  it('validates folder names', () => {
    expect(folderNameIssue('')).toBe('required');
    expect(folderNameIssue('/abs')).toBe('slash');
    expect(folderNameIssue('a/../b')).toBe('dots');
    expect(folderNameIssue('a//b')).toBe('dots');
    expect(folderNameIssue('photos 2026/')).toBeNull();
  });
});

describe('bucketNameIssue (TD-U-04)', () => {
  it.each([
    ['', 'required'],
    ['ab', 'length'],
    ['a'.repeat(64), 'length'],
    ['My-Bucket', 'characters'],
    ['under_score', 'characters'],
    ['-bucket', 'edges'],
    ['bucket.', 'edges'],
    ['my..bucket', 'dots'],
    ['my.-bucket', 'dots'],
    ['192.168.1.1', 'ipAddress'],
    ['xn--bucket', 'reserved'],
    ['nexo', 'reserved'],
    ['logs-s3alias', 'reserved'],
    ['my-bucket.2026', null],
  ] as const)('%s -> %s', (name, issue) => {
    expect(bucketNameIssue(name)).toBe(issue);
  });
});

describe('size units', () => {
  it('converts both ways', () => {
    expect(toBytes(1.5, 'GiB')).toBe(1.5 * 1024 ** 3);
    expect(fromBytes(100 * 1024 ** 3)).toEqual({ value: 100, unit: 'GiB' });
    expect(fromBytes(1536 * 1024 ** 2)).toEqual({ value: 1.5, unit: 'GiB' });
    expect(fromBytes(2 * 1024 ** 4)).toEqual({ value: 2, unit: 'TiB' });
    expect(fromBytes(512 * 1024)).toEqual({ value: 0.5, unit: 'MiB' });
  });
});
