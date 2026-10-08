import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { server } from '@/test/server';
import { api, problemResponse } from '@/test/handlers';

import { ApiError } from '../errors';
import { createHttpClient, resolveUrl } from './client';
import { readCookie } from './csrf';

const baseUrl = '/api/v1';

describe('resolveUrl', () => {
  it('joins base and path against the document origin and appends defined query params', () => {
    const url = new URL(resolveUrl('/api/v1/', '/buckets', { force: true, skip: undefined, n: 2 }));
    expect(url.origin).toBe(window.location.origin);
    expect(url.pathname).toBe('/api/v1/buckets');
    expect(url.searchParams.get('force')).toBe('true');
    expect(url.searchParams.get('n')).toBe('2');
    expect(url.searchParams.has('skip')).toBe(false);
  });

  it('accepts an absolute base URL', () => {
    expect(resolveUrl('https://nexo.example/api/v1', 'info')).toBe(
      'https://nexo.example/api/v1/info',
    );
  });
});

describe('readCookie', () => {
  it('finds a cookie by exact name and decodes it', () => {
    expect(readCookie('nexo_csrf', 'a=1; nexo_csrf=tok%20en; b=2')).toBe('tok en');
    expect(readCookie('nexo_csrf', 'xnexo_csrf=no')).toBeUndefined();
    expect(readCookie('nexo_csrf', '')).toBeUndefined();
  });
});

describe('createHttpClient', () => {
  it('decodes JSON bodies', async () => {
    server.use(http.get(api('/info'), () => HttpResponse.json({ version: '1.2.3' })));
    const client = createHttpClient({ baseUrl });
    await expect(client.request<{ version: string }>('/info')).resolves.toEqual({
      version: '1.2.3',
    });
  });

  it('returns undefined for 204 responses', async () => {
    server.use(http.delete(api('/buckets/x'), () => new HttpResponse(null, { status: 204 })));
    const client = createHttpClient({ baseUrl });
    await expect(client.request('/buckets/x', { method: 'DELETE' })).resolves.toBeUndefined();
  });

  it('turns problem+json responses into ApiError', async () => {
    server.use(
      http.get(api('/buckets/missing'), () =>
        problemResponse(404, 'bucket_not_found', 'Bucket missing does not exist.'),
      ),
    );
    const client = createHttpClient({ baseUrl });
    const err = await client.request('/buckets/missing').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    const apiErr = err as ApiError;
    expect(apiErr.status).toBe(404);
    expect(apiErr.code).toBe('bucket_not_found');
    expect(apiErr.detail).toBe('Bucket missing does not exist.');
    expect(apiErr.requestId).toBe('req-test-1');
  });

  it('falls back to a status error when the body is not a problem document', async () => {
    server.use(
      http.get(
        api('/info'),
        () =>
          new HttpResponse('<html>bad gateway</html>', { status: 502, statusText: 'Bad Gateway' }),
      ),
    );
    const client = createHttpClient({ baseUrl });
    const err = (await client.request('/info').catch((e: unknown) => e)) as ApiError;
    expect(err.status).toBe(502);
    expect(err.code).toBe('http_error');
  });

  it('wraps network failures', async () => {
    server.use(http.get(api('/info'), () => HttpResponse.error()));
    const client = createHttpClient({ baseUrl });
    const err = (await client.request('/info').catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err.isNetwork).toBe(true);
  });

  it('sends the CSRF token from the cookie on mutating requests only', async () => {
    const seen: Record<string, string | null> = {};
    server.use(
      http.post(api('/buckets'), ({ request }) => {
        seen.post = request.headers.get('x-csrf-token');
        return HttpResponse.json({ name: 'b' }, { status: 201 });
      }),
      http.get(api('/buckets'), ({ request }) => {
        seen.get = request.headers.get('x-csrf-token');
        return HttpResponse.json([]);
      }),
    );
    document.cookie = 'nexo_csrf=cookie-token; Path=/';
    const client = createHttpClient({ baseUrl });
    await client.request('/buckets', { method: 'POST', body: { name: 'b' } });
    await client.request('/buckets');
    expect(seen.post).toBe('cookie-token');
    expect(seen.get).toBeNull();
  });

  it('uses the in-memory token when the cookie is not readable', async () => {
    let header: string | null = null;
    server.use(
      http.post(api('/buckets'), ({ request }) => {
        header = request.headers.get('x-csrf-token');
        return HttpResponse.json({ name: 'b' }, { status: 201 });
      }),
    );
    const client = createHttpClient({ baseUrl, readCookie: () => undefined });
    client.setCsrfToken('memory-token');
    await client.request('/buckets', { method: 'POST', body: { name: 'b' } });
    expect(header).toBe('memory-token');
  });

  it('raises onUnauthorized on 401 unless the request opts out', async () => {
    server.use(
      http.get(api('/info'), () => problemResponse(401, 'invalid_access_key')),
      http.post(api('/auth/login'), () => problemResponse(401, 'invalid_access_key')),
    );
    const onUnauthorized = vi.fn();
    const client = createHttpClient({ baseUrl, onUnauthorized });
    await expect(client.request('/info')).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    await expect(
      client.request('/auth/login', { method: 'POST', body: {}, notifyUnauthorized: false }),
    ).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });
});
