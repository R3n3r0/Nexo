import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { api, problemResponse } from '@/test/handlers';
import { server } from '@/test/server';

import { createHttpServices } from './index';

const job = {
  id: 'heal-1',
  state: 'running',
  request: { bucket: 'photos', recursive: true },
  finished: false,
  startedAt: '2026-10-08T10:00:00Z',
  updatedAt: '2026-10-08T10:00:00Z',
  scanned: 0,
  healed: 0,
  failed: 0,
  dangling: 0,
  removed: 0,
  bytes: 0,
  drives: [],
  items: [],
};

describe('HttpHealService', () => {
  it('starts a job with POST /heal and reads it back', async () => {
    let body: unknown;
    server.use(
      http.post(api('/heal'), async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(job, { status: 202 });
      }),
      http.get(api('/heal/heal-1'), () =>
        HttpResponse.json({ ...job, state: 'done', finished: true }),
      ),
      http.get(api('/heal'), ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        return HttpResponse.json(
          cursor === null ? { items: [job], nextCursor: 'next' } : { items: [], nextCursor: '' },
        );
      }),
    );
    const { heal } = createHttpServices({ baseUrl: '/api/v1' });
    await expect(heal.start({ bucket: 'photos', recursive: true })).resolves.toMatchObject({
      id: 'heal-1',
    });
    expect(body).toEqual({ bucket: 'photos', recursive: true });
    await expect(heal.get('heal-1')).resolves.toMatchObject({ state: 'done' });
    await expect(heal.list()).resolves.toHaveLength(1);
  });

  it('rejects with the problem of a single-drive server (501)', async () => {
    server.use(http.get(api('/heal'), () => problemResponse(501, 'not_implemented')));
    const { heal } = createHttpServices({ baseUrl: '/api/v1' });
    await expect(heal.list()).rejects.toMatchObject({ status: 501 });
  });
});
