import { describe, expect, it } from 'vitest';

import { createMockServices } from './index';

const accounts = { nexoadmin: 'nexoadmin', alice: 'alice-secret' };

describe('mock IAM state', () => {
  it('keeps users, groups, policies and entities consistent', async () => {
    const s = createMockServices();
    await s.policies.create({
      name: 'photos-read',
      document: JSON.stringify({
        Version: '2012-10-17',
        Statement: [{ Effect: 'Allow', Action: 's3:GetObject', Resource: 'arn:aws:s3:::photos/*' }],
      }),
    });
    await s.policies.attach({ policyNames: ['photos-read'], user: 'alice' });
    await s.groups.update('developers', {
      policyNames: ['readwrite', 'photos-read'],
      addMembers: ['ci-bot'],
    });
    expect(await s.policies.entities('photos-read')).toEqual({
      policy: 'photos-read',
      users: ['alice'],
      groups: ['developers'],
    });
    expect((await s.users.get('ci-bot')).groups).toEqual(['developers']);

    await s.users.update('alice', { groups: [] });
    expect((await s.groups.get('developers')).members).toEqual(['ci-bot']);

    await s.policies.remove('photos-read');
    expect((await s.users.get('alice')).policyNames).not.toContain('photos-read');
  });

  it('rejects malformed policies with located errors', async () => {
    const s = createMockServices();
    await expect(
      s.policies.save({
        name: 'bad',
        document: '{\n  "Version": "2012-10-17",\n  "Statement": []\n}',
      }),
    ).rejects.toMatchObject({
      status: 400,
      code: 'malformed_policy',
      errors: [expect.objectContaining({ element: 'Statement', line: 3 })],
    });
    await expect(s.policies.create({ name: 'readonly', document: '{}' })).rejects.toMatchObject({
      status: 409,
    });
  });

  it('simulates with the deciding statement', async () => {
    const s = createMockServices();
    await s.policies.save({
      name: 'deny-tmp',
      document: {
        Version: '2012-10-17',
        Statement: [
          { Sid: 'NoTmp', Effect: 'Deny', Action: ['s3:*'], Resource: ['arn:aws:s3:::tmp/*'] },
        ],
      },
    });
    await s.users.update('alice', { policyNames: ['readwrite', 'deny-tmp'] });
    const denied = await s.policies.simulate({
      user: 'alice',
      action: 's3:PutObject',
      bucket: 'tmp',
      object: 'x',
    });
    expect(denied).toMatchObject({
      allowed: false,
      decision: 'deny',
      source: 'iam',
      policyName: 'deny-tmp',
      statement: { sid: 'NoTmp', effect: 'Deny', matched: true },
    });
    const allowed = await s.policies.simulate({
      user: 'alice',
      action: 's3:GetObject',
      bucket: 'media',
    });
    expect(allowed).toMatchObject({ allowed: true, source: 'iam', policyName: 'readwrite' });
    const implicit = await s.policies.simulate({ user: 'alice', action: 'admin:ListUsers' });
    expect(implicit).toMatchObject({ allowed: false, decision: 'deny' });
    expect(implicit.source).toBeUndefined();
  });

  it('evaluates conditions in the simulator', async () => {
    const s = createMockServices();
    await s.policies.save({
      name: 'office',
      document: {
        Version: '2012-10-17',
        Statement: [
          {
            Effect: 'Allow',
            Action: ['s3:GetObject'],
            Resource: ['arn:aws:s3:::*'],
            Condition: { IpAddress: { 'aws:SourceIp': ['10.0.0.0/8'] } },
          },
        ],
      },
    });
    await s.users.update('ci-bot', { policyNames: ['office'] });
    const inside = await s.policies.simulate({
      user: 'ci-bot',
      action: 's3:GetObject',
      bucket: 'b',
      sourceIp: '10.1.2.3',
    });
    const outside = await s.policies.simulate({
      user: 'ci-bot',
      action: 's3:GetObject',
      bucket: 'b',
      sourceIp: '192.168.1.1',
    });
    expect(inside.allowed).toBe(true);
    expect(outside.allowed).toBe(false);
    expect(outside.evaluated[0]?.mismatch).toBe('condition');
  });

  it('enforces admin actions for a signed-in user without them', async () => {
    const s = createMockServices({ accounts, initialAccessKey: 'alice' });
    await expect(s.users.list()).rejects.toMatchObject({ status: 403, code: 'access_denied' });
    await expect(s.config.list()).rejects.toMatchObject({ status: 403 });
    const perms = await s.permissions.get();
    expect(perms).toMatchObject({ user: 'alice', isRoot: false });
    expect(perms.allowed).toContain('s3:GetObject');
    expect(perms.denied).toContain('admin:ListUsers');
    // Self-service: alice manages her own access keys only.
    const key = await s.accessKeys.create({ name: 'laptop' });
    expect(key.parentUser).toBe('alice');
    expect((await s.accessKeys.list()).map((k) => k.accessKey)).toEqual([key.accessKey]);
    await expect(s.accessKeys.list({ user: 'ci-bot' })).rejects.toMatchObject({ status: 403 });
    await expect(s.accessKeys.remove('AKIA0MOCKDEPLOY0001')).rejects.toMatchObject({ status: 403 });
  });

  it('lets users created in the demo sign in', async () => {
    const s = createMockServices();
    await s.users.create({
      accessKey: 'carol',
      secretKey: 'carol-secret',
      policyNames: ['readonly'],
    });
    const session = await s.session.login('carol', 'carol-secret');
    expect(session.identity).toMatchObject({ isRoot: false, policyNames: ['readonly'] });
    await expect(s.session.login('carol', 'wrong-secret')).rejects.toMatchObject({ status: 401 });
  });
});

describe('mock shares', () => {
  it('creates, lists, revokes and enforces the 7 days limit', async () => {
    const s = createMockServices({ now: () => new Date('2026-10-08T12:00:00Z') });
    const share = await s.shares.create({
      bucket: 'media',
      key: 'a/b c.txt',
      expiresIn: 3600,
      contentDisposition: 'attachment; filename="b c.txt"',
    });
    expect(share).toMatchObject({
      owner: 'nexoadmin',
      permission: 'download',
      expiresAt: '2026-10-08T13:00:00.000Z',
    });
    expect(share.url).toContain('/media/a/b%20c.txt?');
    expect(share.url).toContain('response-content-disposition=');
    expect((await s.shares.list({ bucket: 'media' })).map((x) => x.id)).toContain(share.id);
    await expect(
      s.shares.create({ bucket: 'media', key: 'x', expiresIn: 604_801 }),
    ).rejects.toMatchObject({ status: 400 });
    await s.shares.revoke(share.id);
    await expect(s.shares.get(share.id)).rejects.toMatchObject({ status: 404 });
  });

  it('shows a non-admin only its own links (TD-P-07)', async () => {
    const s = createMockServices({ accounts, initialAccessKey: 'alice' });
    expect((await s.shares.list()).map((x) => x.owner)).toEqual(['alice']);
    await expect(s.shares.list({ owner: 'nexoadmin' })).rejects.toMatchObject({ status: 403 });
    await expect(s.shares.revoke('shr0001')).rejects.toMatchObject({ status: 403 });
    await expect(s.shares.revokeAll()).resolves.toBe(1);
  });

  it('signs presigned URLs and POST policies', async () => {
    const s = createMockServices();
    const put = await s.shares.presign({
      method: 'PUT',
      bucket: 'b',
      key: 'k',
      contentType: 'text/plain',
      expiresIn: 60,
    });
    expect(put).toMatchObject({ method: 'PUT', headers: { 'Content-Type': 'text/plain' } });
    expect(put.url).toContain('X-Amz-Expires=60');
    const post = await s.shares.postPolicy({ bucket: 'b', keyPrefix: 'up/', maxSize: 1024 });
    expect(post.fields.key).toBe('up/${filename}');
    expect(post.conditions).toContainEqual({ operator: 'content-length-range', min: 0, max: 1024 });
  });
});

describe('mock config', () => {
  it('masks secrets, keeps them when the mask is sent back and validates', async () => {
    const s = createMockServices();
    const webhook = await s.config.get('notify_webhook');
    expect(webhook.settings.auth_token).toBe('********');
    await s.config.set('notify_webhook', { auth_token: '********', enable: 'on' });
    expect((await s.config.exportAll()).notify_webhook).toEqual({
      enable: 'on',
      auth_token: 'demo-token',
    });
    await expect(s.config.set('logger', { level: 'loud' })).rejects.toMatchObject({ status: 400 });
    await expect(s.config.set('nope', {})).rejects.toMatchObject({ status: 400 });
    await s.config.reset('notify_webhook', ['enable']);
    expect((await s.config.exportAll()).notify_webhook).toEqual({ auth_token: 'demo-token' });
    await s.config.importAll({ region: { name: 'eu-south-1' } });
    expect(await s.config.exportAll()).toEqual({ region: { name: 'eu-south-1' } });
  });
});
