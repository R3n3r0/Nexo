import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { api, problemResponse } from '@/test/handlers';
import { server } from '@/test/server';

import { isApiError } from '../errors';
import { createHttpServices } from './index';

/** Records method, path, query and JSON body of every handled request. */
function recorder() {
  const calls: Array<{ method: string; path: string; query: string; body: unknown }> = [];
  const record = async (request: Request) => {
    const url = new URL(request.url);
    const text = request.method === 'GET' ? '' : await request.text();
    calls.push({
      method: request.method,
      path: url.pathname.replace('/api/v1', ''),
      query: url.search,
      body: text === '' ? undefined : (JSON.parse(text) as unknown),
    });
  };
  return { calls, record };
}

describe('HTTP IAM services', () => {
  it('follows nextCursor to list every user', async () => {
    const cursors: Array<string | null> = [];
    server.use(
      http.get(api('/users'), ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        cursors.push(cursor);
        return cursor === null
          ? HttpResponse.json({ items: [{ accessKey: 'a' }], nextCursor: 'c2' })
          : HttpResponse.json({ items: [{ accessKey: 'b' }] });
      }),
    );
    const services = createHttpServices({ baseUrl: '/api/v1' });
    await expect(services.users.list()).resolves.toEqual([{ accessKey: 'a' }, { accessKey: 'b' }]);
    expect(cursors).toEqual([null, 'c2']);
  });

  it('calls the documented user, group and policy endpoints', async () => {
    const { calls, record } = recorder();
    const ok = (body: Record<string, unknown> = {}) => HttpResponse.json(body);
    server.use(
      http.get(api('/users/:user'), async ({ request }) => (await record(request), ok())),
      http.put(api('/users/:user'), async ({ request }) => (await record(request), ok())),
      http.get(api('/groups/:group'), async ({ request }) => (await record(request), ok())),
      http.put(api('/groups/:group'), async ({ request }) => (await record(request), ok())),
      http.post(api('/policies'), async ({ request }) => (await record(request), ok())),
      http.post(api('/policies/validate'), async ({ request }) => {
        await record(request);
        return ok({ valid: true, errors: [] });
      }),
      http.post(api('/policies/simulate'), async ({ request }) => (await record(request), ok())),
      http.get(
        api('/policies/:policy/entities'),
        async ({ request }) => (await record(request), ok()),
      ),
      http.post(api('/policies/attach'), async ({ request }) => {
        await record(request);
        return new HttpResponse(null, { status: 204 });
      }),
      http.post(api('/policies/detach'), async ({ request }) => {
        await record(request);
        return new HttpResponse(null, { status: 204 });
      }),
      http.get(api('/me/permissions'), async ({ request }) => (await record(request), ok())),
    );
    const s = createHttpServices({ baseUrl: '/api/v1', readCookie: () => undefined });
    await s.users.get('a b');
    await s.users.update('alice', { status: 'disabled', groups: ['g'] });
    await s.groups.get('devs');
    await s.groups.update('devs', { addMembers: ['bob'] });
    await s.policies.create({ name: 'p', document: '{"Version":"2012-10-17"}' });
    await expect(s.policies.validate({ document: '{}' })).resolves.toEqual({
      valid: true,
      errors: [],
    });
    await s.policies.simulate({ action: 's3:GetObject', bucket: 'b' });
    await s.policies.entities('readonly');
    await s.policies.attach({ policyNames: ['p'], user: 'alice' });
    await s.policies.detach({ policyNames: ['p'], group: 'devs' });
    await s.permissions.get({ bucket: 'photos', prefix: 'a/' });
    expect(calls).toEqual([
      { method: 'GET', path: '/users/a%20b', query: '', body: undefined },
      {
        method: 'PUT',
        path: '/users/alice',
        query: '',
        body: { status: 'disabled', groups: ['g'] },
      },
      { method: 'GET', path: '/groups/devs', query: '', body: undefined },
      { method: 'PUT', path: '/groups/devs', query: '', body: { addMembers: ['bob'] } },
      {
        method: 'POST',
        path: '/policies',
        query: '',
        body: { name: 'p', document: '{"Version":"2012-10-17"}' },
      },
      { method: 'POST', path: '/policies/validate', query: '', body: { document: '{}' } },
      {
        method: 'POST',
        path: '/policies/simulate',
        query: '',
        body: { action: 's3:GetObject', bucket: 'b' },
      },
      { method: 'GET', path: '/policies/readonly/entities', query: '', body: undefined },
      {
        method: 'POST',
        path: '/policies/attach',
        query: '',
        body: { policyNames: ['p'], user: 'alice' },
      },
      {
        method: 'POST',
        path: '/policies/detach',
        query: '',
        body: { policyNames: ['p'], group: 'devs' },
      },
      {
        method: 'GET',
        path: '/me/permissions',
        query: '?bucket=photos&prefix=a%2F',
        body: undefined,
      },
    ]);
  });

  it('turns a malformed_policy problem into an ApiError with located errors', async () => {
    server.use(
      http.put(api('/policies/p'), () =>
        problemResponse(400, 'malformed_policy', 'Statement 1: Effect: must be "Allow" or "Deny"', {
          errors: [
            {
              statement: 1,
              element: 'Effect',
              line: 4,
              column: 17,
              message: 'must be "Allow" or "Deny"',
            },
          ],
        }),
      ),
    );
    const s = createHttpServices({ baseUrl: '/api/v1' });
    const error: unknown = await s.policies
      .save({ name: 'p', document: '{}' })
      .catch((e: unknown) => e);
    expect(isApiError(error)).toBe(true);
    if (!isApiError(error)) return;
    expect(error).toMatchObject({ status: 400, code: 'malformed_policy' });
    expect(error.errors).toEqual([
      {
        statement: 1,
        element: 'Effect',
        line: 4,
        column: 17,
        message: 'must be "Allow" or "Deny"',
      },
    ]);
  });

  it('lists access keys of a user and updates them', async () => {
    const { calls, record } = recorder();
    server.use(
      http.get(api('/access-keys'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ items: [] });
      }),
      http.put(api('/access-keys/:key'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ accessKey: 'K' });
      }),
    );
    const s = createHttpServices({ baseUrl: '/api/v1', readCookie: () => undefined });
    await s.accessKeys.list();
    await s.accessKeys.list({ user: 'bob' });
    await s.accessKeys.update('K', { clearExpiration: true });
    expect(calls.map((c) => `${c.method} ${c.path}${c.query}`)).toEqual([
      'GET /access-keys',
      'GET /access-keys?user=bob',
      'PUT /access-keys/K',
    ]);
    expect(calls[2]?.body).toEqual({ clearExpiration: true });
  });
});

describe('HTTP share and config services', () => {
  it('calls the share endpoints', async () => {
    const { calls, record } = recorder();
    server.use(
      http.get(api('/shares'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ items: [{ id: 's1' }] });
      }),
      http.post(api('/shares'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ id: 's2' }, { status: 201 });
      }),
      http.delete(api('/shares'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ revoked: 3 });
      }),
      http.delete(api('/shares/:id'), async ({ request }) => {
        await record(request);
        return new HttpResponse(null, { status: 204 });
      }),
      http.post(api('/presign'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ url: 'u', method: 'GET', expiresAt: 'x', headers: {} });
      }),
      http.post(api('/post-policy'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ url: 'u', fields: {}, expiresAt: 'x', conditions: [] });
      }),
    );
    const s = createHttpServices({ baseUrl: '/api/v1', readCookie: () => undefined });
    await expect(s.shares.list({ owner: 'bob', bucket: 'b' })).resolves.toEqual([{ id: 's1' }]);
    await s.shares.create({ bucket: 'b', key: 'k', expiresIn: 3600 });
    await s.shares.revoke('s1');
    await expect(s.shares.revokeAll('bob')).resolves.toBe(3);
    await s.shares.presign({ bucket: 'b', key: 'k', method: 'PUT' });
    await s.shares.postPolicy({ bucket: 'b', keyPrefix: 'up/' });
    expect(calls.map((c) => `${c.method} ${c.path}${c.query}`)).toEqual([
      'GET /shares?owner=bob&bucket=b',
      'POST /shares',
      'DELETE /shares/s1',
      'DELETE /shares?owner=bob',
      'POST /presign',
      'POST /post-policy',
    ]);
    expect(calls[1]?.body).toEqual({ bucket: 'b', key: 'k', expiresIn: 3600 });
  });

  it('calls the config endpoints', async () => {
    const { calls, record } = recorder();
    server.use(
      http.get(
        api('/config'),
        async ({ request }) => (await record(request), HttpResponse.json([])),
      ),
      http.get(api('/config/schema'), async ({ request }) => {
        await record(request);
        return HttpResponse.json([]);
      }),
      http.get(api('/config/export'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ region: { name: 'x' } });
      }),
      http.post(api('/config/import'), async ({ request }) => {
        await record(request);
        return new HttpResponse(null, { status: 204 });
      }),
      http.put(api('/config/:subsys'), async ({ request }) => {
        await record(request);
        return HttpResponse.json({ name: 'region', settings: {} });
      }),
      http.delete(api('/config/:subsys'), async ({ request }) => {
        await record(request);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const s = createHttpServices({ baseUrl: '/api/v1', readCookie: () => undefined });
    await s.config.list();
    await s.config.schema();
    await expect(s.config.exportAll()).resolves.toEqual({ region: { name: 'x' } });
    await s.config.importAll({ region: { name: 'y' } });
    await s.config.set('region', { name: 'eu-west-1' });
    await s.config.reset('logger', ['level', 'format']);
    await s.config.reset('logger');
    expect(calls.map((c) => `${c.method} ${c.path}${c.query}`)).toEqual([
      'GET /config',
      'GET /config/schema',
      'GET /config/export',
      'POST /config/import',
      'PUT /config/region',
      'DELETE /config/logger?keys=level%2Cformat',
      'DELETE /config/logger',
    ]);
    expect(calls[4]?.body).toEqual({ settings: { name: 'eu-west-1' } });
  });
});
