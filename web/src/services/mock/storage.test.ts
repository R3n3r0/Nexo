import { describe, expect, it } from 'vitest';

import { createMockServices, sampleBuckets } from './index';

const bucket = (name: string, versioning: 'off' | 'enabled' = 'off') => ({
  ...sampleBuckets()[0]!,
  name,
  versioning,
  objects: 0,
  versions: 0,
  size: 0,
  quotaBytes: 0,
  tags: {},
});

describe('mock storage services', () => {
  it('lists folders and objects under a prefix with cursor pagination', async () => {
    const keys = Array.from({ length: 2500 }, (_, i) => ({
      key: `logs/${String(i).padStart(4, '0')}.txt`,
    }));
    const services = createMockServices({
      buckets: [bucket('data')],
      objects: {
        data: [...keys, { key: 'logs/old/a.txt' }, { key: 'readme.md' }, { key: 'logs/' }],
      },
    });
    const root = await services.objects.list('data');
    expect(root).toMatchObject({ prefixes: ['logs/'], isTruncated: false });
    expect(root.objects.map((o) => o.key)).toEqual(['readme.md']);

    const seen: string[] = [];
    let cursor: string | undefined;
    let pages = 0;
    do {
      const page = await services.objects.list('data', {
        prefix: 'logs/',
        ...(cursor ? { cursor } : {}),
      });
      seen.push(...page.prefixes, ...page.objects.map((o) => o.key));
      cursor = page.nextCursor;
      pages += 1;
    } while (cursor !== undefined);
    expect(pages).toBe(3);
    // 2500 objects + the "old/" folder; the folder placeholder "logs/" itself is hidden.
    expect(seen).toHaveLength(2501);
    expect(new Set(seen).size).toBe(2501);
    expect(seen).toContain('logs/old/');

    const flat = await services.objects.list('data', { prefix: 'logs/old', delimiter: '' });
    expect(flat.objects.map((o) => o.key)).toEqual(['logs/old/a.txt']);
  });

  it('keeps versions, delete markers and permanent deletions', async () => {
    const services = createMockServices({ buckets: [bucket('ver', 'enabled')], objects: {} });
    const put = (text: string) =>
      services.uploads.putObject({ bucket: 'ver', key: 'doc.txt', body: new Blob([text]) });
    await put('one');
    await put('two!');
    const deleted = await services.objects.remove('ver', { objects: [{ key: 'doc.txt' }] });
    expect(deleted.deleted[0]).toMatchObject({ key: 'doc.txt', deleteMarker: true });
    expect((await services.objects.list('ver')).objects).toEqual([]);

    const versions = await services.objects.versions('ver', 'doc.txt');
    expect(versions.items.map((v) => [v.isDeleteMarker, v.isLatest, v.size])).toEqual([
      [true, true, 0],
      [false, false, 4],
      [false, false, 3],
    ]);
    // Removing the delete marker restores the previous version.
    await services.objects.remove('ver', {
      objects: [{ key: 'doc.txt', versionId: versions.items[0]?.versionId ?? '' }],
    });
    expect((await services.objects.stat('ver', 'doc.txt')).size).toBe(4);

    await services.objects.remove('ver', { prefixes: [''], allVersions: true });
    expect((await services.objects.versions('ver', 'doc.txt')).items).toEqual([]);
    expect(await services.buckets.get('ver')).toMatchObject({ objects: 0, size: 0 });
  });

  it('moves a folder and refuses to overwrite an object', async () => {
    const services = createMockServices({
      buckets: [bucket('mv')],
      objects: {
        mv: [{ key: 'a/1.txt' }, { key: 'a/sub/2.txt' }, { key: 'b.txt' }, { key: 'c.txt' }],
      },
    });
    const moved = await services.objects.rename('mv', {
      source: 'a/',
      destination: 'z/',
      prefix: true,
    });
    expect(moved.renamed.map((r) => r.destination)).toEqual(['z/1.txt', 'z/sub/2.txt']);
    expect((await services.objects.list('mv')).prefixes).toEqual(['z/']);
    await expect(
      services.objects.rename('mv', { source: 'b.txt', destination: 'c.txt' }),
    ).rejects.toMatchObject({
      status: 409,
    });
    await services.objects.rename('mv', { source: 'b.txt', destination: 'c.txt', overwrite: true });
    expect((await services.objects.list('mv')).objects.map((o) => o.key)).toEqual(['c.txt']);
  });

  it('validates the parts of a multipart upload', async () => {
    const services = createMockServices({
      buckets: [bucket('mp')],
      objects: {},
      uploadLimits: { partSize: 4, minPartSize: 4, maxPartSize: 8, maxParts: 100 },
    });
    const up = await services.uploads.create('mp', 'k', { contentType: 'text/plain' });
    expect(up).toMatchObject({ partSize: 4, minPartSize: 4, maxParts: 100 });
    const p1 = await services.uploads.uploadPart({
      bucket: 'mp',
      key: 'k',
      uploadId: up.uploadId,
      partNumber: 1,
      body: new Blob(['ab']),
    });
    const p2 = await services.uploads.uploadPart({
      bucket: 'mp',
      key: 'k',
      uploadId: up.uploadId,
      partNumber: 2,
      body: new Blob(['cd']),
    });
    await expect(
      services.uploads.complete('mp', 'k', up.uploadId, [
        { partNumber: 2, etag: p2.etag },
        { partNumber: 1, etag: p1.etag },
      ]),
    ).rejects.toMatchObject({ code: 'invalid_part_order' });
    await expect(
      services.uploads.complete('mp', 'k', up.uploadId, [
        { partNumber: 1, etag: p1.etag },
        { partNumber: 2, etag: p2.etag },
      ]),
    ).rejects.toMatchObject({ code: 'entity_too_small' });
    expect((await services.uploads.list('mp')).items).toHaveLength(1);
    await services.uploads.abort('mp', 'k', up.uploadId);
    expect((await services.uploads.list('mp')).items).toEqual([]);
  });

  it('applies anonymous presets per prefix and validates the policy text', async () => {
    const services = createMockServices({ buckets: [bucket('pub')], objects: {} });
    const access = await services.buckets.setAccess('pub', {
      prefix: 'public/',
      preset: 'download',
    });
    expect(access).toMatchObject({
      public: true,
      rules: [{ prefix: 'public/', preset: 'download' }],
    });
    expect(await services.buckets.getPolicy('pub')).toContain('arn:aws:s3:::pub/public/*');
    expect((await services.buckets.get('pub')).public).toBe(true);
    await services.buckets.removeAccess('pub', 'public/');
    expect(await services.buckets.getPolicy('pub')).toBeNull();

    await expect(services.buckets.setPolicy('pub', '{\n  "Statement": [\n}')).rejects.toMatchObject(
      {
        code: 'malformed_policy',
        errors: [expect.objectContaining({ line: 3 })],
      },
    );
  });

  it('reports the usage per bucket', async () => {
    const services = createMockServices();
    const usage = await services.info.usage();
    expect(usage.bucketsCount).toBe(3);
    expect(usage.buckets.media?.objectsCount).toBe(11500);
  });
});
