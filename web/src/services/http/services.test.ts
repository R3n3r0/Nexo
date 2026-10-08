import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { api, problemResponse, sampleSession, testCredentials } from '@/test/handlers';
import { server } from '@/test/server';

import { createHttpServices } from './index';

describe('createHttpServices', () => {
  it('logs in, remembers the CSRF token from the payload and logs out', async () => {
    let logoutHeader: string | null = null;
    server.use(
      http.post(api('/auth/logout'), ({ request }) => {
        logoutHeader = request.headers.get('x-csrf-token');
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const services = createHttpServices({ baseUrl: '/api/v1', readCookie: () => undefined });
    const session = await services.session.login(
      testCredentials.accessKey,
      testCredentials.secretKey,
    );
    expect(session).toEqual(sampleSession());
    await services.session.logout();
    expect(logoutHeader).toBe('csrf-from-login');
  });

  it('rejects a wrong password with a 401 ApiError and no global event', async () => {
    const services = createHttpServices({ baseUrl: '/api/v1' });
    const listener = vi.fn();
    services.authEvents.onUnauthorized(listener);
    await expect(services.session.login('admin', 'wrong-password')).rejects.toMatchObject({
      status: 401,
      code: 'invalid_credentials',
    });
    expect(listener).not.toHaveBeenCalled();
  });

  it('broadcasts a 401 from any service through authEvents', async () => {
    server.use(http.get(api('/buckets'), () => problemResponse(401, 'invalid_access_key')));
    const services = createHttpServices({ baseUrl: '/api/v1' });
    const listener = vi.fn();
    const unsubscribe = services.authEvents.onUnauthorized(listener);
    await expect(services.buckets.list()).rejects.toMatchObject({ status: 401 });
    expect(listener).toHaveBeenCalledTimes(1);
    unsubscribe();
    await expect(services.session.current()).rejects.toMatchObject({ status: 401 });
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('calls the documented endpoints', async () => {
    const calls: string[] = [];
    server.use(
      http.get(api('/buckets'), () => HttpResponse.json({ items: [] })),
      http.post(api('/buckets'), () => HttpResponse.json({ name: 'new' }, { status: 201 })),
      http.delete(api('/buckets/old'), ({ request }) => {
        calls.push(new URL(request.url).search);
        return new HttpResponse(null, { status: 204 });
      }),
      http.get(api('/users'), () => HttpResponse.json({ items: [] })),
      http.get(api('/groups'), () => HttpResponse.json({ items: [] })),
      http.get(api('/policies'), () => HttpResponse.json({ items: [] })),
      http.put(api('/policies/p1'), () => HttpResponse.json({ name: 'p1' })),
      http.get(api('/access-keys'), () => HttpResponse.json({ items: [] })),
    );
    const services = createHttpServices({ baseUrl: '/api/v1' });
    await expect(services.buckets.list()).resolves.toEqual([]);
    await expect(services.buckets.create({ name: 'new' })).resolves.toEqual({ name: 'new' });
    await services.buckets.remove('old', { force: true });
    expect(calls).toEqual(['?force=true']);
    await expect(services.users.list()).resolves.toEqual([]);
    await expect(services.groups.list()).resolves.toEqual([]);
    await expect(services.policies.list()).resolves.toEqual([]);
    await expect(
      services.policies.save({ name: 'p1', document: { Version: '2012-10-17', Statement: [] } }),
    ).resolves.toEqual({ name: 'p1' });
    await expect(services.accessKeys.list()).resolves.toEqual([]);
    await expect(services.info.serverInfo()).resolves.toMatchObject({ mode: 'snsd' });
  });
});
