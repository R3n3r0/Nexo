import { isPublicAllow, presetRulePrefix, validateRule } from '@/lib/accessRules';
import { compileKeyPattern, normalizeKey } from '@/lib/keyPattern';

import { ApiError, type AccessRuleProblem } from '../errors';
import type { AccessRuleService } from '../interfaces';
import type {
  AccessRules,
  AccessRulesKeyResult,
  AccessRulesPreset,
  BlockPublic,
  BlockPublicMode,
  BucketAccessRule,
  EvaluatedAccessRule,
  TestAccessRulesInput,
} from '../types';
import type { MockBucketState, MockStorage } from './storage';
import { clone, problem } from './support';

/** The access rules of a mock bucket (MockBucketState.access). */
export interface MockAccessRulesState {
  rules: BucketAccessRule[];
  /** Version of the rules; the etag is derived from it. */
  version: number;
  updatedAt?: string;
  updatedBy?: string;
  blockPublic: boolean;
}

/** The cluster-level block public access of the mock deployment. */
export interface MockBlockPublicState {
  enabled: boolean;
  mode: BlockPublicMode;
}

export interface MockAccessRuleOptions {
  /** Maximum number of rules of a bucket (200, like --access-rules-max). */
  maxAccessRules?: number;
}

/** Hooks of the other mocks: the caller, its groups and the root account. */
export interface MockAccessRuleDeps {
  /** The access key of the signed-in principal. */
  current: () => string | undefined;
  /** The account the mocks treat as root. */
  rootAccessKey: string;
  /** Groups of a user, for the group: principals. */
  groupsOf?: (user: string) => string[];
  /** Authorizes an action of the signed-in principal on a bucket. */
  allowed?: (action: string, bucket?: string) => boolean;
}

const etagOf = (bucket: string, version: number) =>
  `${bucket.length.toString(16)}${version.toString(16).padStart(8, '0')}`;

function accessOf(state: MockBucketState): MockAccessRulesState {
  state.access ??= { rules: [], version: 0, blockPublic: false };
  return state.access;
}

/** The block public access in effect on a bucket. */
function blockedOn(cluster: MockBlockPublicState, state: MockBucketState): boolean {
  return cluster.enabled || (state.access?.blockPublic ?? false);
}

/** True when an allow rule for everyone is in effect on the bucket. */
export function publicByRules(cluster: MockBlockPublicState, state: MockBucketState): boolean {
  if (blockedOn(cluster, state)) return false;
  return (state.access?.rules ?? []).some(isPublicAllow);
}

/** Actions of each class (accessrule package documentation). */
const classActions: Record<string, string[]> = {
  read: [
    's3:GetObject',
    's3:GetObjectVersion',
    's3:GetObjectAttributes',
    's3:GetObjectVersionAttributes',
    'nexo:GetMediaStream',
  ],
  stream: ['nexo:GetMediaStream'],
  list: ['s3:ListBucket', 's3:ListBucketVersions', 's3:ListBucketMultipartUploads'],
  write: ['s3:PutObject', 's3:AbortMultipartUpload', 's3:ListMultipartUploadParts'],
  delete: ['s3:DeleteObject', 's3:DeleteObjectVersion'],
  tag: [
    's3:GetObjectTagging',
    's3:PutObjectTagging',
    's3:DeleteObjectTagging',
    's3:GetObjectVersionTagging',
    's3:PutObjectVersionTagging',
    's3:DeleteObjectVersionTagging',
  ],
};

/** The S3 action a test asks for (a class stands for its main action). */
function testAction(action: string | undefined): string {
  switch (action ?? 'read') {
    case 'read':
      return 's3:GetObject';
    case 'stream':
      return 'nexo:GetMediaStream';
    case 'list':
      return 's3:ListBucket';
    case 'write':
      return 's3:PutObject';
    case 'delete':
      return 's3:DeleteObject';
    case 'tag':
      return 's3:PutObjectTagging';
    default:
      return action ?? 's3:GetObject';
  }
}

function methodOf(action: string): string {
  if (action === 's3:PutObject' || action.startsWith('s3:Put')) return 'PUT';
  if (action.startsWith('s3:Delete') || action === 's3:AbortMultipartUpload') return 'DELETE';
  return 'GET';
}

/** Matches with the "*" and "?" wildcards, case-sensitive unless asked. */
function wildcard(pattern: string, value: string, insensitive = false): boolean {
  const source = pattern
    .split('')
    .map((ch) => (ch === '*' ? '.*' : ch === '?' ? '.' : ch.replace(/[\\^$.|+()[\]{}]/g, '\\$&')))
    .join('');
  return new RegExp(`^${source}$`, insensitive ? 'is' : 's').test(value);
}

function actionMatches(ruleActions: readonly string[], action: string): boolean {
  return ruleActions.some((a) => {
    if (a === 'all') return Object.values(classActions).some((list) => list.includes(action));
    const list = classActions[a];
    if (list !== undefined) return list.includes(action);
    return wildcard(a, action);
  });
}

/** The 128-bit value of an IPv4 (mapped) or IPv6 address, or undefined. */
function ipValue(address: string): bigint | undefined {
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(address);
  if (v4 !== null) {
    const parts = v4.slice(1).map(Number);
    if (parts.some((p) => p > 255)) return undefined;
    return parts.reduce((acc, p) => (acc << 8n) | BigInt(p), 0xffffn << 32n);
  }
  if (!address.includes(':')) return undefined;
  const [head = '', tail] = address.split('::');
  const h = head === '' ? [] : head.split(':');
  const t = tail === undefined || tail === '' ? [] : tail.split(':');
  const missing = 8 - h.length - t.length;
  if (tail === undefined ? missing !== 0 : missing < 0) return undefined;
  const groups = [...h, ...Array<string>(Math.max(0, missing)).fill('0'), ...t];
  let value = 0n;
  for (const g of groups) {
    if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return undefined;
    value = (value << 16n) | BigInt(Number.parseInt(g, 16));
  }
  return value;
}

function inCidr(ip: string, cidr: string): boolean {
  const [address = '', bitsText] = cidr.split('/');
  const a = ipValue(ip);
  const b = ipValue(address);
  if (a === undefined || b === undefined) return false;
  const v4 = !address.includes(':');
  const bits = bitsText === undefined ? 128 : Number(bitsText) + (v4 ? 96 : 0);
  if (bits <= 0) return true;
  const shift = BigInt(128 - bits);
  return a >> shift === b >> shift;
}

interface TestRequest {
  key: string;
  action: string;
  method: string;
  /** user:<name>, accesskey:<key>, distribution:<id>, or undefined (anonymous). */
  principal?: string;
  groups: string[];
  user?: string;
  ip?: string;
  host?: string;
  secure?: boolean;
  referer?: string;
  headers: Record<string, string>;
  distribution?: string;
  time: Date;
}

/** Whether a principal of a rule designates the requester. */
function principalMatches(p: string, req: TestRequest): boolean {
  if (p === '*') return true;
  const [kind = '', name = ''] = [p.slice(0, p.indexOf(':')), p.slice(p.indexOf(':') + 1)];
  if (kind === 'user') return req.user !== undefined && wildcard(name, req.user);
  if (kind === 'group') return req.groups.some((g) => wildcard(name, g));
  if (kind === 'accesskey') {
    return (
      req.principal?.startsWith('accesskey:') === true && wildcard(name, req.principal.slice(10))
    );
  }
  if (kind === 'distribution') {
    return req.distribution !== undefined && wildcard(name, req.distribution);
  }
  return false;
}

/**
 * The first condition of a rule that does not hold, or the one that
 * cannot be evaluated (unknown: an allow rule does not apply then, a deny
 * rule does); undefined when they all hold.
 */
function failedCondition(
  rule: BucketAccessRule,
  req: TestRequest,
  captures: Record<string, string>,
): { name: string; unknown: boolean } | undefined {
  const c = rule.conditions;
  if (c === undefined) return undefined;
  if ((c.sourceIp?.length ?? 0) > 0) {
    if (req.ip === undefined || req.ip === '') return { name: 'sourceIp', unknown: true };
    if (!(c.sourceIp ?? []).some((cidr) => inCidr(req.ip ?? '', cidr))) {
      return { name: 'sourceIp', unknown: false };
    }
  }
  if ((c.notSourceIp?.length ?? 0) > 0) {
    if (req.ip === undefined || req.ip === '') return { name: 'notSourceIp', unknown: true };
    if ((c.notSourceIp ?? []).some((cidr) => inCidr(req.ip ?? '', cidr))) {
      return { name: 'notSourceIp', unknown: false };
    }
  }
  if ((c.referer?.length ?? 0) > 0) {
    if (!(c.referer ?? []).some((p) => wildcard(p, req.referer ?? '', true))) {
      return { name: 'referer', unknown: false };
    }
  }
  if ((c.host?.length ?? 0) > 0) {
    const host = (req.host ?? '').toLowerCase().replace(/:\d+$/, '');
    const ok = (c.host ?? []).some((h) => {
      const want = h.toLowerCase();
      return want.startsWith('*.') ? host.endsWith(want.slice(1)) : host === want;
    });
    if (!ok) return { name: 'host', unknown: false };
  }
  if (c.secureTransport !== undefined) {
    if (req.secure === undefined) return { name: 'secureTransport', unknown: true };
    if (req.secure !== c.secureTransport) return { name: 'secureTransport', unknown: false };
  }
  if (c.timeWindow !== undefined) {
    const from = c.timeWindow.from !== undefined ? Date.parse(c.timeWindow.from) : -Infinity;
    const to = c.timeWindow.to !== undefined ? Date.parse(c.timeWindow.to) : Infinity;
    const now = req.time.getTime();
    if (now < from || now > to) return { name: 'timeWindow', unknown: false };
  }
  if ((c.methods?.length ?? 0) > 0) {
    if (!(c.methods ?? []).some((m) => m.toUpperCase() === req.method)) {
      return { name: 'methods', unknown: false };
    }
  }
  for (const [name, value] of Object.entries(c.headers ?? {})) {
    if (value.includes('${')) return { name: `headers.${name}`, unknown: true };
    const got = Object.entries(req.headers).find(([k]) => k.toLowerCase() === name.toLowerCase());
    if (got?.[1] !== value) return { name: `headers.${name}`, unknown: false };
  }
  for (const [group, value] of Object.entries(c.captureEquals ?? {})) {
    const want = value.replaceAll('${aws:username}', req.user ?? '\u0000');
    if (want.includes('${') || want.includes('\u0000')) {
      return { name: `captureEquals.${group}`, unknown: true };
    }
    if (captures[group] !== want) return { name: `captureEquals.${group}`, unknown: false };
  }
  return undefined;
}

/** Evaluates the rules of a bucket for one request, like the Evaluator. */
function evaluate(
  rules: readonly BucketAccessRule[],
  req: TestRequest,
  blocked: boolean,
): Omit<AccessRulesKeyResult, 'allowed' | 'reason' | 'source' | 'determinant'> {
  const normalized = normalizeKey(req.key);
  const order = rules
    .map((rule, index) => ({ rule, index }))
    .sort(
      (a, b) =>
        (a.rule.effect === 'deny' ? 0 : 1) - (b.rule.effect === 'deny' ? 0 : 1) ||
        a.rule.priority - b.rule.priority ||
        a.index - b.index,
    );
  const evaluated: EvaluatedAccessRule[] = [];
  const matched: string[] = [];
  const blockedPublic: string[] = [];
  let outcome: AccessRulesKeyResult['rulesOutcome'] = 'none';
  for (const { rule } of order) {
    const base: EvaluatedAccessRule = {
      id: rule.id,
      priority: rule.priority,
      effect: rule.effect === 'allow' ? 'Allow' : 'Deny',
      matched: false,
    };
    const principals = rule.principals.filter((p) => principalMatches(p, req));
    if (principals.length === 0) {
      evaluated.push({ ...base, mismatch: 'principal' });
      continue;
    }
    if (blocked && rule.effect === 'allow' && principals.every((p) => p === '*')) {
      blockedPublic.push(rule.id);
      evaluated.push({ ...base, mismatch: 'blocked-public' });
      continue;
    }
    if (!actionMatches(rule.actions, req.action)) {
      evaluated.push({ ...base, mismatch: 'action' });
      continue;
    }
    let captures: Record<string, string> = {};
    if (rule.match !== undefined) {
      const compiled = compileKeyPattern(rule.match);
      const m = compiled.ok
        ? compiled.compiled.match(normalized)
        : { matched: false, captures: {} };
      if (!m.matched) {
        evaluated.push({ ...base, mismatch: 'key' });
        continue;
      }
      captures = m.captures;
    }
    const failed = failedCondition(rule, req, captures);
    const applies = failed === undefined || (failed.unknown && rule.effect === 'deny');
    if (!applies) {
      evaluated.push({
        ...base,
        ...(Object.keys(captures).length > 0 ? { captures } : {}),
        mismatch: 'condition',
        condition: failed?.name ?? '',
      });
      continue;
    }
    evaluated.push({
      ...base,
      ...(Object.keys(captures).length > 0 ? { captures } : {}),
      matched: true,
    });
    matched.push(rule.id);
    if (outcome === 'none') outcome = rule.effect;
  }
  return {
    key: req.key,
    normalizedKey: normalized,
    rules: evaluated,
    matched,
    ...(blockedPublic.length > 0 ? { blockedPublic } : {}),
    rulesOutcome: outcome,
  };
}

const presets: readonly AccessRulesPreset[] = [
  'private',
  'public-read',
  'public-read-list',
  'public-prefix',
  'public-write',
];

function isPreset(value: string): value is AccessRulesPreset {
  return (presets as readonly string[]).includes(value);
}

/** The invalid_access_rule problem of a rule. */
function invalidRule(code: string, status: number, errors: AccessRuleProblem[]): ApiError {
  return new ApiError({
    title: status === 403 ? 'Forbidden' : 'Bad Request',
    status,
    code,
    detail: errors[0]?.message ?? 'The access rules are not valid.',
    accessRuleErrors: errors,
  });
}

/** Validates the rules like accessrule.Validate (the editor checks, plus duplicates). */
function validate(rules: readonly BucketAccessRule[], max: number, rejectPublic: boolean): void {
  if (rules.length > max) {
    throw invalidRule('too_many_access_rules', 400, [
      { rule: 0, message: `at most ${String(max)} access rules per bucket` },
    ]);
  }
  rules.forEach((rule, index) => {
    const others = rules.filter((_, i) => i !== index);
    const issues = validateRule(rule, others);
    const at = { rule: index + 1, id: rule.id };
    if (issues.pattern !== undefined) {
      throw invalidRule('invalid_access_rule', 400, [
        {
          ...at,
          field: 'match.pattern',
          ...(issues.pattern.position > 0 ? { position: issues.pattern.position } : {}),
          message: `rule ${rule.id}: ${issues.pattern.message}`,
        },
      ]);
    }
    const field = Object.keys(issues)[0];
    if (field !== undefined) {
      throw invalidRule('invalid_access_rule', 400, [
        { ...at, field, message: `rule ${rule.id}: invalid ${field}` },
      ]);
    }
    if (rejectPublic && isPublicAllow(rule)) {
      throw invalidRule('public_access_blocked', 403, [
        {
          ...at,
          field: 'principals',
          message: `rule ${rule.id}: public access is blocked: an allow rule cannot name "*"`,
        },
      ]);
    }
  });
}

/** In-memory AccessRuleService over the mock buckets. */
export function createMockAccessRuleService(
  storage: MockStorage,
  deps: MockAccessRuleDeps,
  options: MockAccessRuleOptions = {},
): AccessRuleService {
  const { ctx } = storage;
  const cluster = storage.blockPublic;
  const max = options.maxAccessRules ?? 200;
  const guard = (bucket?: string) => {
    if (deps.allowed !== undefined && !deps.allowed('admin:ManageAccessRules', bucket)) {
      throw problem(403, 'access_denied', 'Access denied: admin:ManageAccessRules.');
    }
  };

  function config(name: string, state: MockBucketState): AccessRules {
    const access = accessOf(state);
    const blocked = blockedOn(cluster, state);
    const warnings: string[] = [];
    for (const rule of access.rules) {
      if (
        rule.effect === 'allow' &&
        rule.principals.includes('*') &&
        rule.actions.some(
          (a) => ['write', 'delete', 'all'].includes(a) || /^s3:(Put|Delete)/.test(a),
        )
      ) {
        warnings.push(`rule ${rule.id} lets everyone write or delete objects`);
      }
      if (blocked && isPublicAllow(rule)) {
        warnings.push(`rule ${rule.id} is ignored for everyone: public access is blocked`);
      }
    }
    return {
      bucket: name,
      rules: clone(access.rules),
      etag: etagOf(name, access.version),
      ...(access.updatedAt !== undefined ? { updatedAt: access.updatedAt } : {}),
      ...(access.updatedBy !== undefined ? { updatedBy: access.updatedBy } : {}),
      public: publicByRules(cluster, state),
      blockPublic: blocked,
      warnings,
    };
  }

  function put(name: string, rules: BucketAccessRule[], ifMatch: string | undefined): AccessRules {
    const state = storage.bucket(name);
    const access = accessOf(state);
    if (ifMatch !== undefined && ifMatch !== '' && ifMatch !== etagOf(name, access.version)) {
      throw problem(412, 'precondition_failed', 'The access rules changed since they were read.');
    }
    const blocked = blockedOn(cluster, state);
    validate(rules, max, blocked && cluster.mode === 'reject');
    access.rules = clone(rules);
    access.version += 1;
    access.updatedAt = ctx.nowIso();
    access.updatedBy = deps.current() ?? '';
    return config(name, state);
  }

  function blockPublic(state?: MockBucketState): BlockPublic {
    const bucketLevel = state?.access?.blockPublic ?? false;
    return {
      ...(state !== undefined ? { bucket: state.bucket.name } : {}),
      enabled: state === undefined ? cluster.enabled : bucketLevel,
      mode: cluster.mode,
      cluster: cluster.enabled,
      effective: cluster.enabled || bucketLevel,
    };
  }

  return {
    async get(bucket) {
      await ctx.delay();
      guard(bucket);
      return config(bucket, storage.bucket(bucket));
    },
    async set(bucket, rules, ifMatch) {
      await ctx.delay();
      guard(bucket);
      return put(bucket, rules, ifMatch);
    },
    async test(bucket, input: TestAccessRulesInput) {
      await ctx.delay();
      guard(bucket);
      const state = storage.bucket(bucket);
      const rules = accessOf(state).rules;
      const blocked = blockedOn(cluster, state);
      const action = testAction(input.action);
      const principal =
        input.principal === undefined || input.principal === 'anonymous'
          ? undefined
          : input.principal.includes(':')
            ? input.principal
            : `accesskey:${input.principal}`;
      const user = principal?.startsWith('user:') === true ? principal.slice(5) : undefined;
      const isRoot = user === deps.rootAccessKey;
      const keys = input.keys !== undefined && input.keys.length > 0 ? input.keys : [''];
      return {
        bucket,
        action,
        blockPublic: blockPublic(state),
        results: keys.map((key) => {
          const evaluated = evaluate(
            rules,
            {
              key,
              action,
              method: (input.method ?? methodOf(action)).toUpperCase(),
              ...(principal !== undefined ? { principal } : {}),
              ...(user !== undefined ? { user } : {}),
              groups: user !== undefined ? (deps.groupsOf?.(user) ?? []) : [],
              ...(input.ip !== undefined && input.ip !== '' ? { ip: input.ip } : {}),
              ...(input.host !== undefined && input.host !== '' ? { host: input.host } : {}),
              ...(input.secure !== undefined ? { secure: input.secure } : {}),
              ...(input.referer !== undefined ? { referer: input.referer } : {}),
              headers: input.headers ?? {},
              ...(input.distribution !== undefined && input.distribution !== ''
                ? { distribution: input.distribution }
                : {}),
              time: input.time !== undefined ? new Date(input.time) : ctx.now(),
            },
            blocked,
          );
          const first = evaluated.matched[0];
          if (isRoot) {
            return {
              ...evaluated,
              allowed: true,
              source: 'root',
              reason: 'The root user is allowed every action.',
            };
          }
          if (evaluated.rulesOutcome === 'none' || first === undefined) {
            return {
              ...evaluated,
              allowed: false,
              reason: 'No statement or access rule allows the request (implicit deny).',
            };
          }
          const allowed = evaluated.rulesOutcome === 'allow';
          return {
            ...evaluated,
            allowed,
            determinant: first,
            source: 'accessrule',
            reason: `${allowed ? 'Allowed' : 'Denied'} by the access rule ${first}.`,
          };
        }),
      };
    },
    async publicStatus(bucket) {
      await ctx.delay();
      guard(bucket);
      const state = storage.bucket(bucket);
      const rules = accessOf(state).rules.filter(isPublicAllow);
      const ids = rules.map((r) => r.id);
      let preset: AccessRulesPreset = 'private';
      if (rules.length > 0) {
        const suffix = rules.length === 1 ? rules[0]?.id.slice(presetRulePrefix.length) : undefined;
        const generated = rules[0]?.id.startsWith(presetRulePrefix) === true;
        preset = generated && suffix !== undefined && isPreset(suffix) ? suffix : 'custom';
      }
      return {
        bucket,
        public: publicByRules(cluster, state),
        preset,
        ...(ids.length > 0 ? { rules: ids } : {}),
        blocked: blockedOn(cluster, state),
      };
    },
    async applyPreset(bucket, input, ifMatch) {
      await ctx.delay();
      guard(bucket);
      const state = storage.bucket(bucket);
      const generated = (suffix: string, actions: string[]): BucketAccessRule => ({
        id: `${presetRulePrefix}${suffix}`,
        priority: 1000,
        effect: 'allow',
        principals: ['*'],
        actions,
        description: `generated by the ${input.preset} preset`,
      });
      let added: BucketAccessRule[];
      switch (input.preset) {
        case 'private':
          added = [];
          break;
        case 'public-read':
          added = [generated('public-read', ['read'])];
          break;
        case 'public-read-list':
          added = [generated('public-read-list', ['read', 'list'])];
          break;
        case 'public-prefix':
          if (input.match === undefined || input.match.pattern === '') {
            throw problem(
              400,
              'invalid_argument',
              'The public-prefix preset needs a prefix, glob or regex.',
            );
          }
          added = [{ ...generated('public-prefix', ['read']), match: clone(input.match) }];
          break;
        case 'public-write':
          if (input.confirm !== true) {
            throw problem(
              400,
              'confirmation_required',
              'The public-write preset lets anyone upload to the bucket.',
            );
          }
          added = [
            {
              ...generated('public-write', ['write']),
              ...(input.match !== undefined && input.match.pattern !== ''
                ? { match: clone(input.match) }
                : {}),
            },
          ];
          break;
        default:
          throw problem(400, 'invalid_argument', `Unknown preset ${input.preset}.`);
      }
      const kept = accessOf(state).rules.filter(
        (r) =>
          !r.id.startsWith(presetRulePrefix) && !(input.preset === 'private' && isPublicAllow(r)),
      );
      return put(bucket, [...kept, ...added], ifMatch);
    },
    async getBlockPublic(bucket) {
      await ctx.delay();
      guard(bucket);
      return blockPublic(
        bucket === undefined || bucket === '' ? undefined : storage.bucket(bucket),
      );
    },
    async setBlockPublic(input, bucket) {
      await ctx.delay();
      guard(bucket);
      if (bucket === undefined || bucket === '') {
        cluster.enabled = input.enabled;
        if (input.mode !== undefined) cluster.mode = input.mode;
        return blockPublic();
      }
      if (input.mode !== undefined) {
        throw problem(400, 'invalid_argument', 'The mode is a setting of the cluster.');
      }
      const state = storage.bucket(bucket);
      accessOf(state).blockPublic = input.enabled;
      return blockPublic(state);
    },
  };
}
