import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { api } from '@/test/handlers';
import { server } from '@/test/server';

import { ApiError } from '../errors';
import type { MediaConfiguration, MediaStatus } from '../types';
import { createHttpServices } from './index';

/** Video rules, reprocessing and status of the media worker over HTTP. */
describe('media HTTP service', () => {
  it('reads, writes and removes the video rules', async () => {
    const calls: string[] = [];
    const config: MediaConfiguration = {
      rules: [{ id: 'videos', prefix: 'v/', renditions: ['720p'], poster: true }],
    };
    server.use(
      http.get(api('/buckets/a%20b/media'), () => HttpResponse.json(config)),
      http.put(api('/buckets/a%20b/media'), async ({ request }) => {
        calls.push(`PUT ${await request.text()}`);
        return HttpResponse.json(config);
      }),
      http.delete(api('/buckets/a%20b/media'), () => {
        calls.push('DELETE');
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { media } = createHttpServices({ baseUrl: '/api/v1' });
    expect(await media.get('a b')).toEqual(config);
    expect(await media.set('a b', config)).toEqual(config);
    await media.remove('a b');
    expect(calls).toEqual([`PUT ${JSON.stringify(config)}`, 'DELETE']);
  });

  it('decodes the rule errors of a refused configuration', async () => {
    server.use(
      http.put(api('/buckets/b/media'), () =>
        HttpResponse.json(
          {
            title: 'Bad Request',
            status: 400,
            code: 'invalid_configuration',
            detail: 'x',
            ruleErrors: [{ rule: 1, id: 'r', code: 'InvalidArgument', message: 'bad interval' }],
          },
          { status: 400, headers: { 'content-type': 'application/problem+json' } },
        ),
      ),
    );
    const { media } = createHttpServices({ baseUrl: '/api/v1' });
    const error = await media.set('b', { rules: [{ id: 'r' }] }).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).ruleErrors).toEqual([
      { rule: 1, id: 'r', code: 'InvalidArgument', message: 'bad interval' },
    ]);
  });

  it('queues a reprocessing and reads the status of a version', async () => {
    const bodies: unknown[] = [];
    const queries: URLSearchParams[] = [];
    const status: MediaStatus = { state: 'processing', current: true, step: 'renditions' };
    server.use(
      http.post(api('/buckets/b/media/reprocess'), async ({ request }) => {
        bodies.push(await request.json());
        return new HttpResponse(null, { status: 202 });
      }),
      http.get(api('/buckets/b/objects/media/status'), ({ request }) => {
        queries.push(new URL(request.url).searchParams);
        return HttpResponse.json(status);
      }),
    );
    const { media } = createHttpServices({ baseUrl: '/api/v1' });
    await expect(media.reprocess('b', { key: 'a b/c.mp4', versionId: 'v1' })).resolves.toBe(
      undefined,
    );
    expect(bodies).toEqual([{ key: 'a b/c.mp4', versionId: 'v1' }]);
    expect(await media.status('b', 'a b/c.mp4', 'v1')).toEqual(status);
    expect(queries[0]?.get('key')).toBe('a b/c.mp4');
    expect(queries[0]?.get('versionId')).toBe('v1');
    await media.status('b', 'x.mp4');
    expect(queries[1]?.has('versionId')).toBe(false);
  });

  it('builds the poster and thumbnails URLs of a video', () => {
    const { objects } = createHttpServices({ baseUrl: '/api/v1' });
    const poster = new URL(objects.posterUrl('media', 'clips/a b.mp4', 'v2') ?? '');
    expect(poster.pathname).toBe('/api/v1/buckets/media/objects/hls/poster.jpg');
    expect(poster.searchParams.get('key')).toBe('clips/a b.mp4');
    expect(poster.searchParams.get('versionId')).toBe('v2');
    const vtt = new URL(objects.thumbnailsUrl('media', 'x.mp4') ?? '');
    expect(vtt.pathname).toBe('/api/v1/buckets/media/objects/hls/thumbnails.vtt');
    expect(vtt.searchParams.has('versionId')).toBe(false);
  });
});
