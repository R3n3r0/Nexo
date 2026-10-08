import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { api, problemResponse, sampleSession } from '@/test/handlers';
import { server } from '@/test/server';

import { ApiError } from '../errors';
import type { ObjectList } from '../types';
import { createHttpServices } from './index';

/**
 * Services of a signed-in console: the CSRF token comes from the session
 * payload (never from a cookie).
 */
async function signedInServices() {
  server.use(
    http.get(api('/auth/session'), () =>
      HttpResponse.json({ ...sampleSession(), csrfToken: 'csrf-token' }),
    ),
  );
  const services = createHttpServices({ baseUrl: '/api/v1' });
  await services.session.current();
  return services;
}

describe('storage HTTP services (TD-U-14)', () => {
  it('lists a folder with prefix, delimiter and cursor, and keeps special keys intact', async () => {
    const seen: URLSearchParams[] = [];
    const page: ObjectList = {
      objects: [
        {
          key: 'a b/100% #1.txt',
          size: 3,
          etag: 'e',
          lastModified: '2026-10-01T00:00:00Z',
          isDeleteMarker: false,
          isLatest: true,
        },
      ],
      prefixes: ['a b/città/'],
      isTruncated: true,
      nextCursor: 'c2',
    };
    server.use(
      http.get(api('/buckets/media/objects'), ({ request }) => {
        seen.push(new URL(request.url).searchParams);
        return HttpResponse.json(page);
      }),
    );
    const services = createHttpServices({ baseUrl: '/api/v1' });
    const result = await services.objects.list('media', {
      prefix: 'a b/',
      cursor: 'c1',
      limit: 1000,
    });
    expect(result).toEqual(page);
    expect(seen[0]?.get('prefix')).toBe('a b/');
    expect(seen[0]?.get('cursor')).toBe('c1');
    expect(seen[0]?.has('delimiter')).toBe(false);

    await services.objects.list('media', { delimiter: '' });
    expect(seen[1]?.has('delimiter')).toBe(true);
    expect(seen[1]?.get('delimiter')).toBe('');
  });

  it('builds download and preview URLs with the encoded key and version', () => {
    const services = createHttpServices({ baseUrl: '/api/v1' });
    const url = new URL(services.objects.downloadUrl('media', 'a b/c#d%.txt', 'v1'));
    expect(url.pathname).toBe('/api/v1/buckets/media/objects/download');
    expect(url.searchParams.get('key')).toBe('a b/c#d%.txt');
    expect(url.searchParams.get('versionId')).toBe('v1');
    const preview = new URL(services.objects.previewUrl('media', 'x.png'));
    expect(preview.pathname).toBe('/api/v1/buckets/media/objects/preview');
    expect(preview.searchParams.has('versionId')).toBe(false);
  });

  it('reads the head of a text object with a range and reports truncation', async () => {
    let headers: Record<string, string> = {};
    const services = createHttpServices({
      baseUrl: '/api/v1',
      fetch: (_input, init) => {
        headers = { ...(init?.headers as Record<string, string>) };
        return Promise.resolve(
          new Response('hello', {
            status: 206,
            headers: {
              'Content-Type': 'text/plain; charset=utf-8',
              'Content-Range': 'bytes 0-4/1000',
            },
          }),
        );
      },
    });
    const preview = await services.objects.previewText('media', 'log.txt', { maxBytes: 5 });
    expect(headers.Range).toBe('bytes=0-4');
    expect(preview).toEqual({
      text: 'hello',
      truncated: true,
      size: 1000,
      contentType: 'text/plain; charset=utf-8',
    });
  });

  it('previews an empty object (416) as empty text', async () => {
    server.use(
      http.get(api('/buckets/media/objects/preview'), () => problemResponse(416, 'invalid_range')),
    );
    const services = createHttpServices({ baseUrl: '/api/v1' });
    await expect(
      services.objects.previewText('media', 'empty.txt', { maxBytes: 10 }),
    ).resolves.toMatchObject({
      text: '',
      truncated: false,
    });
  });

  it('downloads a zip archive as a form navigation the browser streams to disk', async () => {
    const forms: HTMLFormElement[] = [];
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(function (
      this: HTMLFormElement,
    ) {
      forms.push(this);
    });
    try {
      const services = await signedInServices();
      const done = services.objects.downloadZip('media', {
        keys: ['a.txt'],
        prefixes: ['docs/'],
        base: '',
      });
      expect(submit).toHaveBeenCalledOnce();
      const [form] = forms;
      if (!form) throw new Error('no form submitted');
      expect(form.method).toBe('post');
      expect(new URL(form.action).pathname).toBe('/api/v1/buckets/media/objects/download-zip');
      const fields = Object.fromEntries(
        Array.from(form.elements, (e) => [
          (e as HTMLInputElement).name,
          (e as HTMLInputElement).value,
        ]),
      );
      expect(JSON.parse(fields.request ?? '')).toEqual({
        keys: ['a.txt'],
        prefixes: ['docs/'],
        base: '',
      });
      expect(fields.csrfToken).toBe('csrf-token');
      expect(fields.downloadToken).toMatch(/^[0-9a-f]{32}$/);
      const frame = document.querySelector<HTMLIFrameElement>(`iframe[name="${form.target}"]`);
      expect(frame).not.toBeNull();
      expect(frame?.getAttribute('aria-hidden')).toBe('true');
      expect(document.body.contains(form)).toBe(false);

      // The server answers with the attachment and the started cookie.
      document.cookie = `nexo_dl_${fields.downloadToken ?? ''}=1; path=/`;
      await expect(done).resolves.toBeUndefined();
      expect(document.cookie).not.toContain('nexo_dl_');
    } finally {
      submit.mockRestore();
    }
  });

  it('rejects a refused zip download with the problem the frame loaded', async () => {
    const forms: HTMLFormElement[] = [];
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(function (
      this: HTMLFormElement,
    ) {
      forms.push(this);
    });
    try {
      const services = await signedInServices();
      let unauthorized = 0;
      services.authEvents.onUnauthorized(() => {
        unauthorized += 1;
      });
      const refuse = async (problem: Record<string, unknown>) => {
        const done = services.objects.downloadZip('media', { prefixes: ['private/'] });
        const name = forms.at(-1)?.target ?? '';
        const frame = document.querySelector<HTMLIFrameElement>(`iframe[name="${name}"]`);
        if (!frame?.contentDocument) throw new Error('no download frame');
        // The late load event of the initial blank document is no answer.
        frame.dispatchEvent(new Event('load'));
        expect(document.querySelector(`iframe[name="${name}"]`)).not.toBeNull();
        frame.contentDocument.body.textContent = JSON.stringify(problem);
        frame.dispatchEvent(new Event('load'));
        const error = await done.catch((e: unknown) => e);
        expect(document.querySelector(`iframe[name="${name}"]`)).toBeNull();
        return error as ApiError;
      };
      const denied = await refuse({ title: 'Forbidden', status: 403, code: 'access_denied' });
      expect(denied).toBeInstanceOf(ApiError);
      expect(denied.code).toBe('access_denied');
      expect(denied.status).toBe(403);
      expect(unauthorized).toBe(0);
      const expired = await refuse({ title: 'Unauthorized', status: 401, code: 'unauthenticated' });
      expect(expired.isUnauthorized).toBe(true);
      expect(unauthorized).toBe(1);
    } finally {
      submit.mockRestore();
    }
  });

  it('cancels a zip download that has not started', async () => {
    const forms: HTMLFormElement[] = [];
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(function (
      this: HTMLFormElement,
    ) {
      forms.push(this);
    });
    try {
      const services = await signedInServices();
      const controller = new AbortController();
      const done = services.objects.downloadZip('media', { keys: ['a.txt'] }, controller.signal);
      const name = forms.at(-1)?.target ?? '';
      expect(document.querySelector(`iframe[name="${name}"]`)).not.toBeNull();
      controller.abort();
      const error = await done.catch((e: unknown) => e);
      expect((error as ApiError).isAborted).toBe(true);
      expect(document.querySelector(`iframe[name="${name}"]`)).toBeNull();
    } finally {
      submit.mockRestore();
    }
  });

  it('sends the bucket policy text verbatim and exposes the located errors', async () => {
    let received = '';
    server.use(
      http.put(api('/buckets/media/policy'), async ({ request }) => {
        received = await request.text();
        return problemResponse(400, 'malformed_policy', 'The policy is not valid.', {
          errors: [{ statement: 1, line: 3, column: 7, message: 'unknown action' }],
        });
      }),
    );
    const services = await signedInServices();
    const text = '{\n  "Statement": [\n    {"Action": "s3:Nope"}\n  ]\n}';
    const error = await services.buckets.setPolicy('media', text).catch((e: unknown) => e);
    expect(received).toBe(text);
    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).code).toBe('malformed_policy');
    expect((error as ApiError).errors).toEqual([
      { statement: 1, line: 3, column: 7, message: 'unknown action' },
    ]);
  });

  it('treats missing configurations as empty but not a missing bucket', async () => {
    server.use(
      http.get(api('/buckets/media/policy'), () => problemResponse(404, 'policy_not_found')),
      http.get(api('/buckets/media/encryption'), () => problemResponse(404, 'object_not_found')),
      http.get(api('/buckets/gone/policy'), () => problemResponse(404, 'bucket_not_found')),
      http.get(api('/buckets/media/tags'), () => HttpResponse.json({ tags: {} })),
      http.get(api('/buckets/media/objectlock'), () => HttpResponse.json({ enabled: false })),
      http.get(api('/buckets/docs/policy'), () =>
        HttpResponse.json({ Version: '2012-10-17', Statement: [] }),
      ),
    );
    const services = createHttpServices({ baseUrl: '/api/v1' });
    await expect(services.buckets.getPolicy('media')).resolves.toBeNull();
    await expect(services.buckets.getEncryption('media')).resolves.toBeNull();
    await expect(services.buckets.getTags('media')).resolves.toEqual({});
    await expect(services.buckets.getObjectLock('media')).resolves.toEqual({ enabled: false });
    await expect(services.buckets.getPolicy('gone')).rejects.toMatchObject({
      code: 'bucket_not_found',
    });
    await expect(services.buckets.getPolicy('docs')).resolves.toBe(
      '{\n  "Version": "2012-10-17",\n  "Statement": []\n}',
    );
  });

  it('calls the bucket settings endpoints', async () => {
    const calls: string[] = [];
    const record = (label: string, body: unknown) => {
      calls.push(`${label} ${JSON.stringify(body)}`);
    };
    server.use(
      http.put(api('/buckets/media/versioning'), async ({ request }) => {
        const body = await request.json();
        record('versioning', body);
        return HttpResponse.json(body);
      }),
      http.put(api('/buckets/media/quota'), async ({ request }) => {
        const body = await request.json();
        record('quota', body);
        return HttpResponse.json(body);
      }),
      http.put(api('/buckets/media/tags'), async ({ request }) => {
        const body = await request.json();
        record('tags', body);
        return HttpResponse.json(body);
      }),
      http.put(api('/buckets/media/access'), async ({ request }) => {
        record('access', await request.json());
        return HttpResponse.json({
          bucket: 'media',
          rules: [{ prefix: 'pub/', preset: 'download' }],
          public: true,
          hasPolicy: true,
        });
      }),
      http.delete(api('/buckets/media/access'), ({ request }) => {
        record('unaccess', new URL(request.url).searchParams.get('prefix'));
        return HttpResponse.json({ bucket: 'media', rules: [], public: false, hasPolicy: false });
      }),
    );
    const services = await signedInServices();
    await expect(services.buckets.setVersioning('media', 'suspended')).resolves.toBe('suspended');
    await expect(services.buckets.setQuota('media', 1024)).resolves.toEqual({
      type: 'hard',
      size: 1024,
    });
    await expect(services.buckets.setQuota('media', 0)).resolves.toEqual({ type: '', size: 0 });
    await expect(services.buckets.setTags('media', { team: 'ops' })).resolves.toEqual({
      team: 'ops',
    });
    await expect(
      services.buckets.setAccess('media', { prefix: 'pub/', preset: 'download' }),
    ).resolves.toMatchObject({ public: true });
    await expect(services.buckets.removeAccess('media', 'pub/')).resolves.toMatchObject({
      public: false,
    });
    expect(calls).toEqual([
      'versioning {"status":"suspended"}',
      'quota {"type":"hard","size":1024}',
      'quota {"type":"","size":0}',
      'tags {"tags":{"team":"ops"}}',
      'access {"prefix":"pub/","preset":"download"}',
      'unaccess "pub/"',
    ]);
  });

  it('runs a proxied multipart upload', async () => {
    const calls: string[] = [];
    server.use(
      http.post(api('/buckets/media/uploads'), ({ request }) => {
        calls.push(`create ${new URL(request.url).searchParams.get('key') ?? ''}`);
        return HttpResponse.json(
          {
            bucket: 'media',
            key: 'a b.bin',
            uploadId: 'up/1',
            initiated: '2026-10-08T00:00:00Z',
            partSize: 16,
            minPartSize: 5,
            maxPartSize: 64,
            maxParts: 10000,
          },
          { status: 201 },
        );
      }),
      http.put(api('/buckets/media/uploads/:id/parts/:n'), ({ request, params }) => {
        // jsdom's XMLHttpRequest serialises Blobs of the test runtime as text,
        // so the test checks the headers rather than the bytes.
        calls.push(
          `part ${String(params.id)} ${String(params.n)} ${request.headers.get('content-type') ?? ''} ${request.headers.get('x-csrf-token') ?? ''}`,
        );
        return HttpResponse.json({
          partNumber: Number(params.n),
          etag: `e${String(params.n)}`,
          size: 10,
        });
      }),
      http.post(api('/buckets/media/uploads/:id/complete'), async ({ request }) => {
        calls.push(`complete ${JSON.stringify(await request.json())}`);
        return HttpResponse.json({
          key: 'a b.bin',
          size: 10,
          etag: 'x-2',
          lastModified: '2026-10-08T00:00:00Z',
          isDeleteMarker: false,
          isLatest: true,
        });
      }),
      http.delete(api('/buckets/media/uploads/:id'), ({ request }) => {
        calls.push(`abort ${new URL(request.url).searchParams.get('key') ?? ''}`);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const services = await signedInServices();
    const up = await services.uploads.create('media', 'a b.bin', { contentType: 'text/plain' });
    const part = await services.uploads.uploadPart({
      bucket: 'media',
      key: 'a b.bin',
      uploadId: up.uploadId,
      partNumber: 1,
      body: new Blob(['0123456789']),
    });
    expect(part.etag).toBe('e1');
    await services.uploads.complete('media', 'a b.bin', up.uploadId, [
      { partNumber: 1, etag: 'e1' },
    ]);
    await services.uploads.abort('media', 'a b.bin', up.uploadId);
    expect(calls).toEqual([
      'create a b.bin',
      'part up/1 1 application/octet-stream csrf-token',
      'complete {"parts":[{"partNumber":1,"etag":"e1"}]}',
      'abort a b.bin',
    ]);
  });

  it('turns a problem+json upload failure into an ApiError', async () => {
    server.use(
      http.post(api('/buckets/media/objects/upload'), () =>
        problemResponse(413, 'entity_too_large', 'The body exceeds 67108864 bytes.'),
      ),
    );
    const services = await signedInServices();
    await expect(
      services.uploads.putObject({ bucket: 'media', key: 'big.iso', body: new Blob(['x']) }),
    ).rejects.toMatchObject({ status: 413, code: 'entity_too_large' });
  });

  it('rejects an aborted request with an aborted ApiError', async () => {
    server.use(
      http.get(api('/buckets/media/objects'), () =>
        HttpResponse.json({ objects: [], prefixes: [], isTruncated: false }),
      ),
    );
    const services = createHttpServices({ baseUrl: '/api/v1' });
    const controller = new AbortController();
    controller.abort();
    await expect(
      services.uploads.putObject(
        { bucket: 'media', key: 'k', body: new Blob(['x']) },
        { signal: controller.signal },
      ),
    ).rejects.toMatchObject({ code: 'aborted' });
    await expect(
      services.objects.previewText('media', 'k', { maxBytes: 1, signal: controller.signal }),
    ).rejects.toMatchObject({ code: 'aborted', isAborted: true });
  });

  it('collects every page of the bucket list and reads the usage', async () => {
    server.use(
      http.get(api('/buckets'), ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        return cursor === 'p2'
          ? HttpResponse.json({ items: [{ name: 'b2' }] })
          : HttpResponse.json({ items: [{ name: 'b1' }], nextCursor: 'p2' });
      }),
      http.get(api('/usage'), () =>
        HttpResponse.json({
          lastUpdate: '2026-10-08T00:00:00Z',
          objectsCount: 1,
          versionsCount: 1,
          totalSize: 5,
          bucketsCount: 1,
          buckets: {},
        }),
      ),
    );
    const services = createHttpServices({ baseUrl: '/api/v1' });
    await expect(services.buckets.list()).resolves.toEqual([{ name: 'b1' }, { name: 'b2' }]);
    await expect(services.info.usage()).resolves.toMatchObject({ totalSize: 5 });
  });
});
