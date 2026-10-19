import { corsLimits, validateCorsRule, type CorsIssue } from '@/lib/cors';
import { imagesMediaRule, mediaRenditions } from '@/lib/mediaRules';

import type { BucketService } from '../interfaces';
import type { Bucket, PolicyError } from '../types';
import { ApiError, type RuleProblem } from '../errors';
import { publicByRules } from './accessRules';
import { applyPreset, presetPolicy, type MockBucketState, type MockStorage } from './storage';
import { clone, invalidConfiguration, problem } from './support';

export function sampleBuckets(): Bucket[] {
  return [
    {
      name: 'backups',
      created: '2026-09-01T10:00:00Z',
      region: 'us-east-1',
      versioning: 'enabled',
      objectLockEnabled: false,
      objects: 1240,
      versions: 310,
      deleteMarkers: 4,
      size: 120 * 1024 ** 3,
      quotaBytes: 0,
      public: false,
      tags: { team: 'ops' },
    },
    {
      name: 'media',
      created: '2026-09-12T15:30:00Z',
      region: 'us-east-1',
      versioning: 'off',
      objectLockEnabled: false,
      objects: 11500,
      versions: 0,
      deleteMarkers: 0,
      size: 70 * 1024 ** 3,
      quotaBytes: 100 * 1024 ** 3,
      public: false,
      tags: {},
    },
    {
      name: 'logs-archive',
      created: '2026-10-01T08:00:00Z',
      region: 'us-east-1',
      // Object lock implies versioning (it cannot be suspended).
      versioning: 'enabled',
      objectLockEnabled: true,
      objects: 100,
      versions: 20,
      deleteMarkers: 0,
      size: 4 * 1024 ** 3,
      quotaBytes: 0,
      public: false,
      tags: {},
    },
  ];
}

/**
 * Mirrors domain.ValidateBucketName: 3-63 chars, lowercase letters, digits,
 * dots and hyphens, starting and ending with a letter or digit, no adjacent
 * dots and no dot next to a hyphen, not an IPv4 address.
 */
export function isValidBucketName(name: string): boolean {
  if (name.length < 3 || name.length > 63) return false;
  if (!/^[a-z0-9][a-z0-9.-]*[a-z0-9]$/.test(name)) return false;
  if (name.includes('..') || name.includes('.-') || name.includes('-.')) return false;
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(name)) return false;
  if (name.startsWith('xn--') || name.startsWith('sthree-')) return false;
  if (name.endsWith('-s3alias') || name.endsWith('--ol-s3')) return false;
  return true;
}

export interface MockBucketOptions {
  buckets?: Bucket[];
}

export function createMockBucketService(storage: MockStorage): BucketService {
  const { ctx } = storage;

  /** The bucket as the server reports it: public by its policy or by its access rules. */
  const view = (state: MockBucketState): Bucket => {
    const bucket = clone(state.bucket);
    bucket.public = state.bucket.public === true || publicByRules(storage.blockPublic, state);
    return bucket;
  };

  const service: BucketService = {
    async list() {
      await ctx.delay();
      return [...storage.buckets.values()].map(view).sort((a, b) => a.name.localeCompare(b.name));
    },
    async get(name) {
      await ctx.delay();
      return view(storage.bucket(name));
    },
    async create(input) {
      await ctx.delay();
      if (!isValidBucketName(input.name) || input.name === 'nexo') {
        throw problem(400, 'invalid_bucket_name', `Bucket name ${input.name} is not valid.`);
      }
      if (storage.buckets.has(input.name)) {
        throw problem(409, 'bucket_already_exists', `Bucket ${input.name} already exists.`);
      }
      const bucket: Bucket = {
        name: input.name,
        created: ctx.nowIso(),
        region: input.region ?? 'us-east-1',
        versioning: input.objectLock === true || input.versioning === true ? 'enabled' : 'off',
        objectLockEnabled: input.objectLock ?? false,
        objects: 0,
        versions: 0,
        deleteMarkers: 0,
        size: 0,
        quotaBytes: input.quotaBytes ?? 0,
        public: false,
        tags: {},
      };
      storage.buckets.set(bucket.name, {
        bucket,
        keys: new Map(),
        policy: null,
        rules: [],
        encryption: null,
        // The rules the server sets for the HLS and images options
        // (domain.HLSMediaRule, then domain.ImagesMediaRule).
        ...(input.hls === true || input.images === true
          ? {
              media: [
                ...(input.hls === true
                  ? [
                      {
                        id: 'hls',
                        renditions: [...mediaRenditions],
                        poster: true,
                        thumbnails: { interval: '10s' },
                      },
                    ]
                  : []),
                ...(input.images === true ? [imagesMediaRule()] : []),
              ],
            }
          : {}),
      });
      return clone(bucket);
    },
    async remove(name, removeOptions) {
      await ctx.delay();
      const state = storage.bucket(name);
      if ((state.bucket.objects > 0 || state.keys.size > 0) && removeOptions?.force !== true) {
        throw problem(409, 'bucket_not_empty', `Bucket ${name} is not empty.`);
      }
      storage.buckets.delete(name);
    },
    async getVersioning(name) {
      await ctx.delay();
      return storage.bucket(name).bucket.versioning;
    },
    async setVersioning(name, status) {
      await ctx.delay();
      const state = storage.bucket(name);
      if (state.bucket.objectLockEnabled && status === 'suspended') {
        throw problem(409, 'conflict', 'Versioning cannot be suspended with object lock.');
      }
      state.bucket.versioning = status;
      return status;
    },
    async getQuota(name) {
      await ctx.delay();
      const size = storage.bucket(name).bucket.quotaBytes;
      return { type: size > 0 ? 'hard' : '', size };
    },
    async setQuota(name, sizeBytes) {
      await ctx.delay();
      if (!Number.isFinite(sizeBytes) || sizeBytes < 0) {
        throw problem(400, 'invalid_argument', 'The quota must be a positive size.');
      }
      storage.bucket(name).bucket.quotaBytes = sizeBytes;
      return { type: sizeBytes > 0 ? 'hard' : '', size: sizeBytes };
    },
    async getTags(name) {
      await ctx.delay();
      return { ...storage.bucket(name).bucket.tags };
    },
    async setTags(name, tags) {
      await ctx.delay();
      storage.bucket(name).bucket.tags = { ...tags };
      return { ...tags };
    },
    async getObjectLock(name) {
      await ctx.delay();
      const state = storage.bucket(name);
      if (!state.bucket.objectLockEnabled) return { enabled: false };
      return clone(state.objectLock ?? { enabled: true, mode: 'GOVERNANCE', days: 30 });
    },
    async setObjectLock(name, config) {
      await ctx.delay();
      const state = storage.bucket(name);
      if (!config.enabled) {
        throw problem(400, 'malformed_xml', 'ObjectLockEnabled must be Enabled.');
      }
      if (!state.bucket.objectLockEnabled) {
        throw problem(
          409,
          'conflict',
          'Object Lock can only be enabled when the bucket is created (x-amz-bucket-object-lock-enabled).',
        );
      }
      const hasPeriod = (config.days ?? 0) > 0 || (config.years ?? 0) > 0;
      if (
        ((config.days ?? 0) > 0 && (config.years ?? 0) > 0) ||
        (hasPeriod && config.mode === undefined)
      ) {
        throw problem(400, 'malformed_xml', 'Specify a mode and either days or years.');
      }
      state.bucket.objectLockEnabled = true;
      state.objectLock = clone(config);
      return clone(config);
    },
    async getEncryption(name) {
      await ctx.delay();
      const { encryption } = storage.bucket(name);
      return encryption ? clone(encryption) : null;
    },
    async setEncryption(name, encryption) {
      await ctx.delay();
      const state = storage.bucket(name);
      if (encryption === null) {
        state.encryption = null;
        return null;
      }
      if (encryption.algorithm !== 'AES256' && encryption.algorithm !== 'aws:kms') {
        throw problem(400, 'malformed_xml', 'SSEAlgorithm must be AES256 or aws:kms.');
      }
      if (encryption.algorithm === 'AES256' && (encryption.kmsKeyId ?? '') !== '') {
        throw problem(400, 'invalid_argument', 'KMSMasterKeyID is only valid with aws:kms.');
      }
      state.encryption = clone(encryption);
      return clone(encryption);
    },
    async getPolicy(name) {
      await ctx.delay();
      const state = storage.bucket(name);
      return state.policy ?? presetPolicy(name, state.rules);
    },
    async setPolicy(name, text) {
      await ctx.delay();
      const state = storage.bucket(name);
      const errors = validatePolicyText(text);
      if (errors.length > 0) {
        throw new ApiError({
          title: 'Bad Request',
          status: 400,
          code: 'malformed_policy',
          detail: errors[0]?.message ?? 'The policy is not valid.',
          errors,
        });
      }
      state.policy = text;
      state.rules = text.includes('"*"') ? [{ prefix: '', preset: 'custom' }] : [];
      state.bucket.public = state.rules.length > 0;
    },
    async deletePolicy(name) {
      await ctx.delay();
      const state = storage.bucket(name);
      state.policy = null;
      state.rules = [];
      state.bucket.public = false;
    },
    async getAccess(name) {
      await ctx.delay();
      const state = storage.bucket(name);
      return {
        bucket: name,
        rules: clone(state.rules),
        public: state.rules.length > 0,
        hasPolicy: state.policy !== null || state.rules.length > 0,
      };
    },
    async setAccess(name, input) {
      await ctx.delay();
      const state = storage.bucket(name);
      const prefix = input.prefix ?? '';
      state.rules = applyPreset(
        state.rules.filter((r) => r.preset !== 'custom'),
        prefix,
        input.preset,
      );
      state.policy = null;
      state.bucket.public = state.rules.length > 0;
      return {
        bucket: name,
        rules: clone(state.rules),
        public: state.bucket.public,
        hasPolicy: state.rules.length > 0,
      };
    },
    removeAccess(name, prefix) {
      return service.setAccess(name, { prefix, preset: 'private' });
    },
    async getCors(name) {
      await ctx.delay();
      return { rules: clone(storage.bucket(name).cors ?? []) };
    },
    async setCors(name, config) {
      await ctx.delay();
      const state = storage.bucket(name);
      const problems: RuleProblem[] = [];
      if (config.rules.length > corsLimits.rules) {
        problems.push({
          rule: 0,
          code: 'InvalidRequest',
          message: `The CORS configuration must have at most ${String(corsLimits.rules)} rules`,
        });
      }
      config.rules.forEach((rule, i) => {
        const issues = validateCorsRule(rule);
        const issue: CorsIssue | undefined =
          issues.id ??
          issues.origins ??
          issues.methods ??
          issues.allowedHeaders ??
          issues.maxAgeSeconds;
        if (issue !== undefined) {
          problems.push({
            rule: i + 1,
            ...(rule.id !== undefined ? { id: rule.id } : {}),
            code: issue === 'origins' || issue === 'methods' ? 'MalformedXML' : 'InvalidArgument',
            message: `CORSRule ${String(i + 1)}: ${issue}`,
          });
        }
      });
      if (problems.length > 0) {
        throw invalidConfiguration(problems);
      }
      if (config.rules.length === 0) {
        delete state.cors;
      } else {
        state.cors = clone(config.rules);
      }
      return { rules: clone(config.rules) };
    },
  };
  return service;
}

/**
 * Minimal validation of a policy text: JSON syntax (located by line and
 * column) and a Statement array, like the malformed_policy problems of
 * the server.
 */
export function validatePolicyText(text: string): PolicyError[] {
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    const { line, column } = locate(text, message);
    return [{ statement: 0, line, column, message }];
  }
  if (
    typeof doc !== 'object' ||
    doc === null ||
    !Array.isArray((doc as { Statement?: unknown }).Statement)
  ) {
    return [
      { statement: 0, element: 'Statement', line: 1, column: 1, message: 'Statement is required.' },
    ];
  }
  return [];
}

/** Line and column of a JSON.parse error ("at position N" or "line L column C"). */
function locate(text: string, message: string): { line: number; column: number } {
  const lc = /line (\d+) column (\d+)/.exec(message);
  if (lc?.[1] !== undefined && lc[2] !== undefined) {
    return { line: Number(lc[1]), column: Number(lc[2]) };
  }
  const pos = /position (\d+)/.exec(message);
  const offset = pos?.[1] !== undefined ? Number(pos[1]) : text.length;
  const before = text.slice(0, offset).split('\n');
  return { line: before.length, column: (before[before.length - 1]?.length ?? 0) + 1 };
}
