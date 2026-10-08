import type { AccessKeyService, GroupService, PolicyService, UserService } from '../interfaces';
import type { AccessKey, Group, Policy, User } from '../types';
import { clone, problem, type MockContext } from './support';

export function sampleUsers(): User[] {
  return [
    {
      accessKey: 'alice',
      status: 'enabled',
      policyNames: ['readwrite'],
      groups: ['developers'],
      createdAt: '2026-09-02T09:00:00Z',
    },
    {
      accessKey: 'ci-bot',
      status: 'enabled',
      policyNames: ['writeonly'],
      groups: [],
      createdAt: '2026-09-20T12:00:00Z',
    },
  ];
}

export function sampleGroups(): Group[] {
  return [
    {
      name: 'developers',
      status: 'enabled',
      members: ['alice'],
      policyNames: ['readwrite'],
      createdAt: '2026-09-02T09:05:00Z',
    },
  ];
}

export function samplePolicies(): Policy[] {
  const statement = (actions: string[]) => ({
    Version: '2012-10-17',
    Statement: [{ Effect: 'Allow' as const, Action: actions, Resource: ['arn:aws:s3:::*'] }],
  });
  return [
    {
      name: 'consoleAdmin',
      builtIn: true,
      document: statement(['admin:*', 's3:*']),
      updatedAt: '2026-09-01T00:00:00Z',
    },
    {
      name: 'readwrite',
      builtIn: true,
      document: statement(['s3:*']),
      updatedAt: '2026-09-01T00:00:00Z',
    },
    {
      name: 'readonly',
      builtIn: true,
      document: statement(['s3:GetBucketLocation', 's3:GetObject']),
      updatedAt: '2026-09-01T00:00:00Z',
    },
    {
      name: 'writeonly',
      builtIn: true,
      document: statement(['s3:PutObject']),
      updatedAt: '2026-09-01T00:00:00Z',
    },
  ];
}

export function sampleAccessKeys(): AccessKey[] {
  return [
    {
      accessKey: 'AKIA0MOCKDEPLOY0001',
      parentUser: 'ci-bot',
      name: 'deploy',
      description: 'Used by the CI pipeline',
      status: 'enabled',
      createdAt: '2026-09-21T08:00:00Z',
    },
  ];
}

/** Generic in-memory collection keyed by a string, with the usual IAM errors. */
function collection<T>(items: T[], key: (item: T) => string, notFoundCode: string, label: string) {
  const store = new Map<string, T>(items.map((item) => [key(item), clone(item)]));
  return {
    list(): T[] {
      return [...store.values()].sort((a, b) => key(a).localeCompare(key(b))).map(clone);
    },
    get(id: string): T {
      const item = store.get(id);
      if (!item) {
        throw problem(404, notFoundCode, `${label} ${id} does not exist.`);
      }
      return clone(item);
    },
    add(item: T, conflictCode: string): T {
      if (store.has(key(item))) {
        throw problem(409, conflictCode, `${label} ${key(item)} already exists.`);
      }
      store.set(key(item), clone(item));
      return clone(item);
    },
    set(item: T): T {
      store.set(key(item), clone(item));
      return clone(item);
    },
    remove(id: string): void {
      if (!store.delete(id)) {
        throw problem(404, notFoundCode, `${label} ${id} does not exist.`);
      }
    },
  };
}

export interface MockIamOptions {
  users?: User[];
  groups?: Group[];
  policies?: Policy[];
  accessKeys?: AccessKey[];
}

export function createMockUserService(ctx: MockContext, options: MockIamOptions = {}): UserService {
  const users = collection(
    options.users ?? sampleUsers(),
    (u) => u.accessKey,
    'user_not_found',
    'User',
  );
  return {
    async list() {
      await ctx.delay();
      return users.list();
    },
    async create(input) {
      await ctx.delay();
      if (input.accessKey.length < 3) {
        throw problem(400, 'invalid_argument', 'Access key must be at least 3 characters.');
      }
      if (input.secretKey.length < 8) {
        throw problem(400, 'invalid_argument', 'Secret key must be at least 8 characters.');
      }
      return users.add(
        {
          accessKey: input.accessKey,
          status: 'enabled',
          policyNames: input.policyNames ?? [],
          groups: [],
          createdAt: ctx.nowIso(),
        },
        'user_already_exists',
      );
    },
    async remove(accessKey) {
      await ctx.delay();
      users.remove(accessKey);
    },
  };
}

export function createMockGroupService(
  ctx: MockContext,
  options: MockIamOptions = {},
): GroupService {
  const groups = collection(
    options.groups ?? sampleGroups(),
    (g) => g.name,
    'group_not_found',
    'Group',
  );
  return {
    async list() {
      await ctx.delay();
      return groups.list();
    },
    async create(input) {
      await ctx.delay();
      return groups.add(
        {
          name: input.name,
          status: 'enabled',
          members: input.members ?? [],
          policyNames: input.policyNames ?? [],
          createdAt: ctx.nowIso(),
        },
        'conflict',
      );
    },
    async remove(name) {
      await ctx.delay();
      groups.remove(name);
    },
  };
}

export function createMockPolicyService(
  ctx: MockContext,
  options: MockIamOptions = {},
): PolicyService {
  const policies = collection(
    options.policies ?? samplePolicies(),
    (p) => p.name,
    'policy_not_found',
    'Policy',
  );
  return {
    async list() {
      await ctx.delay();
      return policies.list();
    },
    async get(name) {
      await ctx.delay();
      return policies.get(name);
    },
    async save(input) {
      await ctx.delay();
      return policies.set({
        name: input.name,
        document: input.document,
        builtIn: false,
        updatedAt: ctx.nowIso(),
      });
    },
    async remove(name) {
      await ctx.delay();
      const existing = policies.get(name);
      if (existing.builtIn) {
        throw problem(403, 'access_denied', `Policy ${name} is built in and cannot be deleted.`);
      }
      policies.remove(name);
    },
  };
}

export function createMockAccessKeyService(
  ctx: MockContext,
  options: MockIamOptions = {},
): AccessKeyService {
  const keys = collection(
    options.accessKeys ?? sampleAccessKeys(),
    (k) => k.accessKey,
    'user_not_found',
    'Access key',
  );
  let counter = 0;
  return {
    async list() {
      await ctx.delay();
      return keys.list();
    },
    async create(input) {
      await ctx.delay();
      counter += 1;
      const created = keys.add(
        {
          accessKey: `AKIA0MOCK${String(counter).padStart(10, '0')}`,
          parentUser: input.parentUser ?? 'nexoadmin',
          name: input.name ?? '',
          description: input.description ?? '',
          status: 'enabled',
          ...(input.expiration !== undefined ? { expiration: input.expiration } : {}),
          createdAt: ctx.nowIso(),
        },
        'user_already_exists',
      );
      return { ...created, secretKey: `mock-secret-${counter}` };
    },
    async remove(accessKey) {
      await ctx.delay();
      keys.remove(accessKey);
    },
  };
}
