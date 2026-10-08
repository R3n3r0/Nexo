import type { BucketService } from '../interfaces';
import type { Bucket } from '../types';
import { clone, problem, type MockContext } from './support';

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
      size: 120 * 1024 ** 3,
      quotaBytes: 0,
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
      size: 70 * 1024 ** 3,
      quotaBytes: 100 * 1024 ** 3,
      tags: {},
    },
    {
      name: 'logs-archive',
      created: '2026-10-01T08:00:00Z',
      region: 'us-east-1',
      versioning: 'suspended',
      objectLockEnabled: true,
      objects: 100,
      versions: 20,
      size: 4 * 1024 ** 3,
      quotaBytes: 0,
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

export function createMockBucketService(
  ctx: MockContext,
  options: MockBucketOptions = {},
): BucketService {
  const store = new Map<string, Bucket>();
  for (const bucket of options.buckets ?? sampleBuckets()) {
    store.set(bucket.name, clone(bucket));
  }

  return {
    async list() {
      await ctx.delay();
      return [...store.values()].sort((a, b) => a.name.localeCompare(b.name)).map(clone);
    },
    async get(name) {
      await ctx.delay();
      const bucket = store.get(name);
      if (!bucket) {
        throw problem(404, 'bucket_not_found', `Bucket ${name} does not exist.`);
      }
      return clone(bucket);
    },
    async create(input) {
      await ctx.delay();
      if (!isValidBucketName(input.name)) {
        throw problem(400, 'invalid_bucket_name', `Bucket name ${input.name} is not valid.`);
      }
      if (store.has(input.name)) {
        throw problem(409, 'bucket_already_exists', `Bucket ${input.name} already exists.`);
      }
      const bucket: Bucket = {
        name: input.name,
        created: ctx.nowIso(),
        region: input.region ?? 'us-east-1',
        versioning: input.objectLock ? 'enabled' : 'off',
        objectLockEnabled: input.objectLock ?? false,
        objects: 0,
        versions: 0,
        size: 0,
        quotaBytes: 0,
        tags: {},
      };
      store.set(bucket.name, bucket);
      return clone(bucket);
    },
    async remove(name, removeOptions) {
      await ctx.delay();
      const bucket = store.get(name);
      if (!bucket) {
        throw problem(404, 'bucket_not_found', `Bucket ${name} does not exist.`);
      }
      if (bucket.objects > 0 && !removeOptions?.force) {
        throw problem(409, 'bucket_not_empty', `Bucket ${name} is not empty.`);
      }
      store.delete(name);
    },
  };
}
