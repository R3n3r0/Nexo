import { describe, expect, it, vi } from 'vitest';

import { createMockServices, isValidBucketName } from './index';

describe('mock services', () => {
  it('session: rejects unknown credentials, accepts configured ones, expires', async () => {
    const services = createMockServices({ accounts: { admin: 'supersecret1' } });
    await expect(services.session.current()).rejects.toMatchObject({ status: 401 });
    await expect(services.session.login('admin', 'nope')).rejects.toMatchObject({ status: 403 });
    await expect(services.session.login('ghost', 'supersecret1')).rejects.toMatchObject({
      status: 403,
    });
    const session = await services.session.login('admin', 'supersecret1');
    expect(session.accessKey).toBe('admin');
    expect(session.csrfToken).not.toBe('');
    await expect(services.session.current()).resolves.toMatchObject({ id: session.id });

    const listener = vi.fn();
    services.authEvents.onUnauthorized(listener);
    services.session.expire();
    await expect(services.session.current()).rejects.toMatchObject({ status: 401 });
    expect(listener).toHaveBeenCalledTimes(1);

    await services.session.logout();
    await expect(services.session.current()).rejects.toMatchObject({ status: 401 });
  });

  it('session: can start authenticated', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    await expect(services.session.current()).resolves.toMatchObject({ accessKey: 'nexoadmin' });
  });

  it('buckets: create, duplicate, remove, not empty, not found', async () => {
    const services = createMockServices();
    const before = await services.buckets.list();
    const created = await services.buckets.create({ name: 'new-bucket', objectLock: true });
    expect(created).toMatchObject({
      name: 'new-bucket',
      versioning: 'enabled',
      objectLockEnabled: true,
    });
    expect((await services.buckets.list()).map((b) => b.name)).toContain('new-bucket');
    await expect(services.buckets.create({ name: 'new-bucket' })).rejects.toMatchObject({
      status: 409,
      code: 'bucket_already_exists',
    });
    await expect(services.buckets.create({ name: 'Bad_Name' })).rejects.toMatchObject({
      code: 'invalid_bucket_name',
    });
    await expect(services.buckets.remove(before[0]?.name ?? '')).rejects.toMatchObject({
      code: 'bucket_not_empty',
    });
    await services.buckets.remove(before[0]?.name ?? '', { force: true });
    await services.buckets.remove('new-bucket');
    await expect(services.buckets.get('new-bucket')).rejects.toMatchObject({
      code: 'bucket_not_found',
    });
  });

  it('buckets: returned objects are copies', async () => {
    const services = createMockServices();
    const [first] = await services.buckets.list();
    if (!first) throw new Error('expected sample buckets');
    first.name = 'mutated';
    expect((await services.buckets.list()).map((b) => b.name)).not.toContain('mutated');
  });

  it('iam: users, groups, policies and access keys behave like the API', async () => {
    const services = createMockServices();
    const user = await services.users.create({ accessKey: 'bob', secretKey: 'password123' });
    expect(user.status).toBe('enabled');
    await expect(
      services.users.create({ accessKey: 'bob', secretKey: 'password123' }),
    ).rejects.toMatchObject({ code: 'user_already_exists' });
    await expect(
      services.users.create({ accessKey: 'x', secretKey: 'password123' }),
    ).rejects.toMatchObject({ code: 'invalid_argument' });
    await services.users.remove('bob');
    await expect(services.users.remove('bob')).rejects.toMatchObject({ code: 'user_not_found' });

    const group = await services.groups.create({ name: 'qa', members: ['alice'] });
    expect(group.members).toEqual(['alice']);
    await services.groups.remove('qa');

    await expect(services.policies.remove('readonly')).rejects.toMatchObject({ status: 403 });
    const saved = await services.policies.save({
      name: 'custom',
      document: { Version: '2012-10-17', Statement: [] },
    });
    expect(saved.builtIn).toBe(false);
    await services.policies.remove('custom');
    await expect(services.policies.get('custom')).rejects.toMatchObject({
      code: 'policy_not_found',
    });

    const key = await services.accessKeys.create({ name: 'ci' });
    expect(key.secretKey).not.toBe('');
    expect((await services.accessKeys.list()).map((k) => k.accessKey)).toContain(key.accessKey);
    await services.accessKeys.remove(key.accessKey);
  });

  it('applies the configured latency', async () => {
    vi.useFakeTimers();
    try {
      const services = createMockServices({ latencyMs: 100 });
      const pending = services.info.serverInfo();
      let settled = false;
      void pending.then(() => {
        settled = true;
      });
      await vi.advanceTimersByTimeAsync(50);
      expect(settled).toBe(false);
      await vi.advanceTimersByTimeAsync(60);
      expect(settled).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('isValidBucketName', () => {
  it.each(['abc', 'my-bucket', 'a.b.c', 'bucket123'])('accepts %s', (name) => {
    expect(isValidBucketName(name)).toBe(true);
  });

  it.each([
    'ab',
    'A-bucket',
    '-abc',
    'abc-',
    'a..b',
    'a.-b',
    '192.168.1.1',
    'xn--abc',
    'abc-s3alias',
  ])('rejects %s', (name) => {
    expect(isValidBucketName(name)).toBe(false);
  });
});
