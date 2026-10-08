import { consoleActionScopes } from '@/lib/policyCatalog';
import { lintPolicy, policyVersion } from '@/lib/policyLint';

import type {
  AccessKeyService,
  GroupService,
  PermissionService,
  PolicyService,
  UserService,
} from '../interfaces';
import type {
  AccessKey,
  Group,
  Policy,
  PolicyDocument,
  PolicyDocumentOrText,
  Simulation,
  User,
} from '../types';
import { evaluate, type SourcedDocument } from './evaluate';
import { clone, problem, type MockContext } from './support';

export function sampleUsers(): User[] {
  return [
    {
      accessKey: 'alice',
      status: 'enabled',
      policyNames: ['readwrite'],
      groups: ['developers'],
      createdAt: '2026-09-02T09:00:00Z',
      updatedAt: '2026-09-02T09:00:00Z',
    },
    {
      accessKey: 'ci-bot',
      status: 'enabled',
      policyNames: ['writeonly'],
      groups: [],
      createdAt: '2026-09-20T12:00:00Z',
      updatedAt: '2026-09-20T12:00:00Z',
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
      updatedAt: '2026-09-02T09:05:00Z',
    },
  ];
}

export function samplePolicies(): Policy[] {
  const statement = (actions: string[]) => ({
    Version: policyVersion,
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
      name: 'diagnostics',
      builtIn: true,
      document: statement([
        'admin:ServerInfo',
        'admin:DataUsageInfo',
        'admin:ServerTrace',
        'admin:ConsoleLog',
        'admin:Prometheus',
      ]),
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
      updatedAt: '2026-09-21T08:00:00Z',
    },
  ];
}

export interface MockIamOptions {
  users?: User[];
  groups?: Group[];
  policies?: Policy[];
  accessKeys?: AccessKey[];
}

/** Who is calling the mock services. */
export interface MockCaller {
  /** Access key of the signed-in principal; null without a session. */
  current(): string | null;
  /** The root account (every action allowed, owns no IAM record). */
  readonly rootAccessKey: string;
}

/** The IAM part of the mock bundle, sharing one in-memory state. */
export interface MockIam {
  users: UserService;
  groups: GroupService;
  policies: PolicyService;
  accessKeys: AccessKeyService;
  permissions: PermissionService;
  /** Groups and effective policies of a principal (mock session identity). */
  identity(accessKey: string): { groups: string[]; policyNames: string[] };
  /** Checks a user's secret (login with users created in the demo). */
  verifySecret(accessKey: string, secretKey: string): boolean;
  /** Whether the caller may perform an action (root and anonymous mocks: always). */
  allowed(action: string, bucket?: string, object?: string): boolean;
  /** Effective caller: the signed-in principal or the root account. */
  caller(): string;
}

const accessKeyPattern = /^[A-Za-z0-9\-_.@+]{3,20}$/;
const policyNamePattern = /^[A-Za-z0-9+=,.@_-]{1,128}$/;

const sortBy = <T>(items: Iterable<T>, key: (item: T) => string): T[] =>
  [...items].sort((a, b) => key(a).localeCompare(key(b)));

/**
 * Builds the IAM mocks over one shared state, so that attaching a policy
 * shows up in the user, the policy entities, the simulator and the
 * effective permissions. Without a session the caller is the root account
 * (unit tests of the services); with a session, admin actions are checked
 * against the caller's policies like the server does.
 */
export function createMockIam(
  ctx: MockContext,
  options: MockIamOptions,
  callerInfo: MockCaller,
): MockIam {
  const users = new Map((options.users ?? sampleUsers()).map((u) => [u.accessKey, clone(u)]));
  const secrets = new Map<string, string>();
  const groups = new Map((options.groups ?? sampleGroups()).map((g) => [g.name, clone(g)]));
  const policies = new Map((options.policies ?? samplePolicies()).map((p) => [p.name, clone(p)]));
  const keys = new Map(
    (options.accessKeys ?? sampleAccessKeys()).map((k) => [k.accessKey, clone(k)]),
  );
  let keyCounter = 0;

  const caller = () => callerInfo.current() ?? callerInfo.rootAccessKey;
  const isRoot = (accessKey: string) => accessKey === callerInfo.rootAccessKey;

  const notFound = (code: string, label: string, id: string) =>
    problem(404, code, `${label} ${id} does not exist.`);
  const getUser = (id: string) =>
    users.get(id) ?? throwError(notFound('user_not_found', 'User', id));
  const getGroup = (id: string) =>
    groups.get(id) ?? throwError(notFound('group_not_found', 'Group', id));
  const getPolicy = (id: string) =>
    policies.get(id) ?? throwError(notFound('policy_not_found', 'Policy', id));
  const getKey = (id: string) =>
    keys.get(id) ?? throwError(notFound('user_not_found', 'Access key', id));

  /** Documents that apply to a principal (user or access key). */
  function documentsOf(principal: string): SourcedDocument[] {
    const key = keys.get(principal);
    const owner = key ? key.parentUser : principal;
    const docs: SourcedDocument[] = [];
    const user = users.get(owner);
    if (user && user.status === 'enabled') {
      for (const name of user.policyNames) {
        const p = policies.get(name);
        if (p) docs.push({ source: 'iam', policyName: name, document: p.document });
      }
      for (const groupName of user.groups) {
        const g = groups.get(groupName);
        if (!g || g.status !== 'enabled') continue;
        for (const name of g.policyNames) {
          const p = policies.get(name);
          if (p)
            docs.push({
              source: 'group',
              policyName: name,
              group: groupName,
              document: p.document,
            });
        }
      }
    }
    if (key?.policy) {
      docs.push({ source: 'inline', document: key.policy });
    }
    return docs;
  }

  function principalIsRoot(principal: string): boolean {
    const key = keys.get(principal);
    return isRoot(key ? key.parentUser : principal) && !key?.policy;
  }

  function simulateFor(
    principal: string,
    action: string,
    bucket?: string,
    object?: string,
    context: Record<string, string[]> = {},
  ): Simulation {
    return evaluate(
      documentsOf(principal),
      {
        user: principal,
        action,
        ...(bucket !== undefined ? { bucket } : {}),
        ...(object !== undefined ? { object } : {}),
        context,
      },
      { root: principalIsRoot(principal) },
    );
  }

  const allowed = (action: string, bucket?: string, object?: string) =>
    simulateFor(caller(), action, bucket, object).allowed;

  /** Throws the 403 the transport answers when the guard action is denied. */
  const guard = (action: string) => {
    if (!allowed(action)) {
      throw problem(403, 'access_denied', `Access denied: ${action} is not allowed.`);
    }
  };

  const checkPolicyNames = (names: readonly string[] | undefined) => {
    for (const name of names ?? []) getPolicy(name);
  };
  const checkGroups = (names: readonly string[] | undefined) => {
    for (const name of names ?? []) getGroup(name);
  };

  function setMembership(user: User, groupNames: string[]) {
    for (const g of groups.values()) {
      const member = g.members.includes(user.accessKey);
      const wanted = groupNames.includes(g.name);
      if (member && !wanted) g.members = g.members.filter((m) => m !== user.accessKey);
      if (!member && wanted) g.members = [...g.members, user.accessKey].sort();
    }
    user.groups = [...groupNames].sort();
  }

  function toDocument(input: PolicyDocumentOrText): PolicyDocument {
    const text = typeof input === 'string' ? input : JSON.stringify(input, null, 2);
    const errors = lintPolicy(text);
    if (errors.length > 0) {
      throw problem(400, 'malformed_policy', errors[0]?.message ?? 'Malformed policy.', {
        errors,
      });
    }
    return JSON.parse(text) as PolicyDocument;
  }

  const userService: UserService = {
    async list() {
      await ctx.delay();
      guard('admin:ListUsers');
      return sortBy(users.values(), (u) => u.accessKey).map(clone);
    },
    async get(accessKey) {
      await ctx.delay();
      guard('admin:GetUser');
      return clone(getUser(accessKey));
    },
    async create(input) {
      await ctx.delay();
      guard('admin:CreateUser');
      if (!accessKeyPattern.test(input.accessKey)) {
        throw problem(400, 'invalid_argument', 'Access key must be at least 3 characters.');
      }
      if (input.secretKey.length < 8 || input.secretKey.length > 40) {
        throw problem(400, 'invalid_argument', 'Secret key must be at least 8 characters.');
      }
      if (users.has(input.accessKey) || isRoot(input.accessKey)) {
        throw problem(409, 'user_already_exists', `User ${input.accessKey} already exists.`);
      }
      checkPolicyNames(input.policyNames);
      checkGroups(input.groups);
      const user: User = {
        accessKey: input.accessKey,
        status: input.status ?? 'enabled',
        policyNames: [...(input.policyNames ?? [])],
        groups: [],
        createdAt: ctx.nowIso(),
        updatedAt: ctx.nowIso(),
      };
      users.set(user.accessKey, user);
      secrets.set(user.accessKey, input.secretKey);
      setMembership(user, input.groups ?? []);
      return clone(user);
    },
    async update(accessKey, input) {
      await ctx.delay();
      const user = getUser(accessKey);
      if (input.status !== undefined && input.status !== user.status) {
        guard(input.status === 'enabled' ? 'admin:EnableUser' : 'admin:DisableUser');
      }
      if (input.secretKey !== undefined) {
        guard('admin:CreateUser');
        if (input.secretKey.length < 8 || input.secretKey.length > 40) {
          throw problem(400, 'invalid_argument', 'Secret key must be 8 to 40 characters.');
        }
        secrets.set(accessKey, input.secretKey);
      }
      if (input.policyNames !== undefined) {
        guard('admin:AttachUserOrGroupPolicy');
        checkPolicyNames(input.policyNames);
        user.policyNames = [...input.policyNames];
      }
      if (input.groups !== undefined) {
        guard('admin:AddUserToGroup');
        checkGroups(input.groups);
        setMembership(user, input.groups);
      }
      if (input.status !== undefined) user.status = input.status;
      user.updatedAt = ctx.nowIso();
      return clone(user);
    },
    async remove(accessKey) {
      await ctx.delay();
      guard('admin:DeleteUser');
      const user = getUser(accessKey);
      setMembership(user, []);
      users.delete(accessKey);
      secrets.delete(accessKey);
      for (const [id, key] of keys) {
        if (key.parentUser === accessKey) keys.delete(id);
      }
    },
  };

  const groupService: GroupService = {
    async list() {
      await ctx.delay();
      guard('admin:ListGroups');
      return sortBy(groups.values(), (g) => g.name).map(clone);
    },
    async get(name) {
      await ctx.delay();
      guard('admin:GetGroup');
      return clone(getGroup(name));
    },
    async create(input) {
      await ctx.delay();
      guard('admin:AddUserToGroup');
      if (input.name.trim() === '' || input.name.includes(',')) {
        throw problem(400, 'invalid_argument', 'Group name must be 1 to 128 characters.');
      }
      if (groups.has(input.name)) {
        throw problem(409, 'conflict', `Group ${input.name} already exists.`);
      }
      checkPolicyNames(input.policyNames);
      for (const member of input.members ?? []) getUser(member);
      const group: Group = {
        name: input.name,
        status: 'enabled',
        members: [...(input.members ?? [])].sort(),
        policyNames: [...(input.policyNames ?? [])],
        createdAt: ctx.nowIso(),
        updatedAt: ctx.nowIso(),
      };
      groups.set(group.name, group);
      for (const member of group.members) {
        const user = getUser(member);
        user.groups = [...new Set([...user.groups, group.name])].sort();
      }
      return clone(group);
    },
    async update(name, input) {
      await ctx.delay();
      const group = getGroup(name);
      if (input.status !== undefined && input.status !== group.status) {
        guard(input.status === 'enabled' ? 'admin:EnableGroup' : 'admin:DisableGroup');
        group.status = input.status;
      }
      if (input.policyNames !== undefined) {
        guard('admin:AttachUserOrGroupPolicy');
        checkPolicyNames(input.policyNames);
        group.policyNames = [...input.policyNames];
      }
      if ((input.addMembers ?? []).length > 0) guard('admin:AddUserToGroup');
      if ((input.removeMembers ?? []).length > 0) guard('admin:RemoveUserFromGroup');
      for (const member of input.addMembers ?? []) {
        const user = getUser(member);
        if (!group.members.includes(member)) group.members = [...group.members, member].sort();
        user.groups = [...new Set([...user.groups, name])].sort();
      }
      for (const member of input.removeMembers ?? []) {
        const user = getUser(member);
        group.members = group.members.filter((m) => m !== member);
        user.groups = user.groups.filter((g) => g !== name);
      }
      group.updatedAt = ctx.nowIso();
      return clone(group);
    },
    async remove(name) {
      await ctx.delay();
      guard('admin:RemoveUserFromGroup');
      const group = getGroup(name);
      for (const member of group.members) {
        const user = users.get(member);
        if (user) user.groups = user.groups.filter((g) => g !== name);
      }
      groups.delete(name);
    },
  };

  const policyService: PolicyService = {
    async list() {
      await ctx.delay();
      guard('admin:ListUserPolicies');
      return sortBy(policies.values(), (p) => p.name).map(clone);
    },
    async get(name) {
      await ctx.delay();
      guard('admin:GetPolicy');
      return clone(getPolicy(name));
    },
    async create(input) {
      await ctx.delay();
      guard('admin:CreatePolicy');
      if (!policyNamePattern.test(input.name)) {
        throw problem(400, 'invalid_argument', `Policy name ${input.name} is not valid.`);
      }
      if (policies.has(input.name)) {
        throw problem(409, 'conflict', `Policy ${input.name} already exists.`);
      }
      const policy: Policy = {
        name: input.name,
        document: toDocument(input.document),
        builtIn: false,
        updatedAt: ctx.nowIso(),
      };
      policies.set(policy.name, policy);
      return clone(policy);
    },
    async save(input) {
      await ctx.delay();
      guard('admin:CreatePolicy');
      const existing = policies.get(input.name);
      if (existing?.builtIn) {
        throw problem(403, 'access_denied', `Policy ${input.name} is built in and read-only.`);
      }
      if (!policyNamePattern.test(input.name)) {
        throw problem(400, 'invalid_argument', `Policy name ${input.name} is not valid.`);
      }
      const policy: Policy = {
        name: input.name,
        document: toDocument(input.document),
        builtIn: false,
        updatedAt: ctx.nowIso(),
      };
      policies.set(policy.name, policy);
      return clone(policy);
    },
    async remove(name) {
      await ctx.delay();
      guard('admin:DeletePolicy');
      const existing = getPolicy(name);
      if (existing.builtIn) {
        throw problem(403, 'access_denied', `Policy ${name} is built in and cannot be deleted.`);
      }
      policies.delete(name);
      for (const user of users.values())
        user.policyNames = user.policyNames.filter((p) => p !== name);
      for (const group of groups.values()) {
        group.policyNames = group.policyNames.filter((p) => p !== name);
      }
    },
    async validate(input) {
      await ctx.delay();
      const text =
        typeof input.document === 'string'
          ? input.document
          : JSON.stringify(input.document, null, 2);
      const errors = lintPolicy(
        text,
        input.bucket !== undefined ? { kind: 'bucket', bucket: input.bucket } : {},
      );
      return { valid: errors.length === 0, errors };
    },
    async simulate(input) {
      await ctx.delay();
      guard('admin:SimulatePolicy');
      const principal = input.user !== undefined && input.user !== '' ? input.user : caller();
      if (!users.has(principal) && !keys.has(principal) && !isRoot(principal)) {
        throw notFound('user_not_found', 'User', principal);
      }
      const context: Record<string, string[]> = { ...(input.conditions ?? {}) };
      if (input.sourceIp !== undefined && input.sourceIp !== '')
        context['aws:SourceIp'] = [input.sourceIp];
      if (input.secureTransport !== undefined) {
        context['aws:SecureTransport'] = [String(input.secureTransport)];
      }
      context['aws:CurrentTime'] = [input.time ?? ctx.nowIso()];
      context['aws:username'] = [principal];
      return clone(simulateFor(principal, input.action, input.bucket, input.object, context));
    },
    async entities(name) {
      await ctx.delay();
      guard('admin:GetPolicy');
      getPolicy(name);
      return {
        policy: name,
        users: sortBy(users.values(), (u) => u.accessKey)
          .filter((u) => u.policyNames.includes(name))
          .map((u) => u.accessKey),
        groups: sortBy(groups.values(), (g) => g.name)
          .filter((g) => g.policyNames.includes(name))
          .map((g) => g.name),
      };
    },
    async attach(input) {
      await ctx.delay();
      guard('admin:AttachUserOrGroupPolicy');
      checkPolicyNames(input.policyNames);
      const target = input.user !== undefined ? getUser(input.user) : getGroup(input.group ?? '');
      target.policyNames = [...new Set([...target.policyNames, ...input.policyNames])];
    },
    async detach(input) {
      await ctx.delay();
      guard('admin:AttachUserOrGroupPolicy');
      const target = input.user !== undefined ? getUser(input.user) : getGroup(input.group ?? '');
      target.policyNames = target.policyNames.filter((p) => !input.policyNames.includes(p));
    },
  };

  /** Self-service rule: own keys always, others' with the admin action. */
  const checkKeyOwner = (parent: string, action: string) => {
    const me = caller();
    const ownerOfMe = keys.get(me)?.parentUser ?? me;
    if (parent !== ownerOfMe) guard(action);
  };

  const accessKeyService: AccessKeyService = {
    async list(filter) {
      await ctx.delay();
      const me = caller();
      const admin = allowed('admin:ListServiceAccounts');
      const user = filter?.user;
      if (user !== undefined && user !== '' && user !== me) {
        guard('admin:ListServiceAccounts');
        if (!users.has(user) && !isRoot(user)) throw notFound('user_not_found', 'User', user);
      }
      const owner = user !== undefined && user !== '' ? user : admin ? undefined : me;
      return sortBy(keys.values(), (k) => k.accessKey)
        .filter((k) => owner === undefined || k.parentUser === owner)
        .map(clone);
    },
    async get(accessKey) {
      await ctx.delay();
      const key = getKey(accessKey);
      checkKeyOwner(key.parentUser, 'admin:ListServiceAccounts');
      return clone(key);
    },
    async create(input) {
      await ctx.delay();
      const parent =
        input.parentUser !== undefined && input.parentUser !== '' ? input.parentUser : caller();
      checkKeyOwner(parent, 'admin:CreateServiceAccount');
      if (!users.has(parent) && !isRoot(parent)) throw notFound('user_not_found', 'User', parent);
      if (input.expiration !== undefined && Date.parse(input.expiration) <= ctx.now().getTime()) {
        throw problem(400, 'invalid_argument', 'The expiration must be in the future.');
      }
      keyCounter += 1;
      const accessKey =
        input.accessKey !== undefined && input.accessKey !== ''
          ? input.accessKey
          : `AKIA0MOCK${String(keyCounter).padStart(10, '0')}`;
      if (!accessKeyPattern.test(accessKey)) {
        throw problem(400, 'invalid_argument', 'Access key must be 3 to 20 characters.');
      }
      if (keys.has(accessKey) || users.has(accessKey)) {
        throw problem(409, 'conflict', `Access key ${accessKey} already exists.`);
      }
      const key: AccessKey = {
        accessKey,
        parentUser: parent,
        name: input.name ?? '',
        description: input.description ?? '',
        status: 'enabled',
        ...(input.expiration !== undefined ? { expiration: input.expiration } : {}),
        ...(input.policy !== undefined ? { policy: toDocument(input.policy) } : {}),
        createdAt: ctx.nowIso(),
        updatedAt: ctx.nowIso(),
      };
      keys.set(accessKey, key);
      const secretKey =
        input.secretKey !== undefined && input.secretKey !== ''
          ? input.secretKey
          : `mock-secret-${String(keyCounter).padStart(4, '0')}-${accessKey.toLowerCase()}`;
      return { ...clone(key), secretKey };
    },
    async update(accessKey, input) {
      await ctx.delay();
      const key = getKey(accessKey);
      checkKeyOwner(key.parentUser, 'admin:UpdateServiceAccount');
      if (input.name !== undefined) key.name = input.name;
      if (input.description !== undefined) key.description = input.description;
      if (input.status !== undefined) key.status = input.status;
      if (input.clearExpiration === true) {
        delete key.expiration;
      } else if (input.expiration !== undefined) {
        if (Date.parse(input.expiration) <= ctx.now().getTime()) {
          throw problem(400, 'invalid_argument', 'The expiration must be in the future.');
        }
        key.expiration = input.expiration;
      }
      if (input.clearPolicy === true) {
        delete key.policy;
      } else if (input.policy !== undefined) {
        key.policy = toDocument(input.policy);
      }
      key.updatedAt = ctx.nowIso();
      return clone(key);
    },
    async remove(accessKey) {
      await ctx.delay();
      const key = getKey(accessKey);
      checkKeyOwner(key.parentUser, 'admin:RemoveServiceAccount');
      keys.delete(accessKey);
    },
  };

  const permissionService: PermissionService = {
    async get(query) {
      await ctx.delay();
      const me = caller();
      const user = query?.user !== undefined && query.user !== '' ? query.user : me;
      if (user !== me) {
        guard('admin:GetUser');
        if (!users.has(user) && !keys.has(user) && !isRoot(user)) {
          throw notFound('user_not_found', 'User', user);
        }
      }
      const bucket = query?.bucket !== undefined && query.bucket !== '' ? query.bucket : undefined;
      const prefix = query?.prefix ?? '';
      const allowedActions: string[] = [];
      const deniedActions: string[] = [];
      for (const [action, scope] of consoleActionScopes) {
        const target =
          scope === 'global'
            ? {}
            : scope === 'bucket'
              ? { bucket: bucket ?? '*' }
              : { bucket: bucket ?? '*', object: `${prefix}*` };
        const ok = simulateFor(
          user,
          action,
          target.bucket,
          'object' in target ? target.object : undefined,
        ).allowed;
        (ok ? allowedActions : deniedActions).push(action);
      }
      return {
        user,
        isRoot: principalIsRoot(user),
        ...(bucket !== undefined ? { bucket } : {}),
        ...(query?.prefix !== undefined ? { prefix: query.prefix } : {}),
        allowed: allowedActions,
        denied: deniedActions,
      };
    },
  };

  return {
    users: userService,
    groups: groupService,
    policies: policyService,
    accessKeys: accessKeyService,
    permissions: permissionService,
    identity(accessKey) {
      const user = users.get(keys.get(accessKey)?.parentUser ?? accessKey);
      if (!user) {
        return { groups: [], policyNames: isRoot(accessKey) ? ['consoleAdmin'] : [] };
      }
      const enabledGroups = user.groups.filter((g) => groups.get(g)?.status === 'enabled');
      const names = new Set(user.policyNames);
      for (const g of enabledGroups) for (const p of groups.get(g)?.policyNames ?? []) names.add(p);
      return { groups: enabledGroups, policyNames: [...names] };
    },
    verifySecret(accessKey, secretKey) {
      const user = users.get(accessKey);
      return (
        user !== undefined && user.status === 'enabled' && secrets.get(accessKey) === secretKey
      );
    },
    allowed,
    caller,
  };
}

function throwError(error: Error): never {
  throw error;
}
