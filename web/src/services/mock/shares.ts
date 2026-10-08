import type { ShareService } from '../interfaces';
import type { PostCondition, PostPolicy, PresignedUrl, Share } from '../types';
import type { MockIam } from './iam';
import { clone, problem, type MockContext } from './support';

/** Maximum validity of share links and presigned URLs (7 days). */
export const maxExpirySeconds = 604_800;

/** Public S3 endpoint the mock links point at. */
const endpoint = 'http://localhost:9000';

export interface MockShareOptions {
  shares?: Share[];
}

export function sampleShares(): Share[] {
  return [
    {
      id: 'shr0001',
      owner: 'nexoadmin',
      bucket: 'media',
      key: 'videos/intro.mp4',
      prefix: false,
      permission: 'download',
      expiresAt: '2026-10-14T10:00:00Z',
      createdAt: '2026-10-07T10:00:00Z',
      url: `${endpoint}/media/videos/intro.mp4?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=604800&X-Amz-Signature=mock`,
      accessKey: 'share-shr0001',
    },
    {
      id: 'shr0002',
      owner: 'alice',
      bucket: 'backups',
      key: 'incoming/',
      prefix: true,
      permission: 'upload',
      expiresAt: '2026-10-09T08:00:00Z',
      createdAt: '2026-10-08T08:00:00Z',
      accessKey: 'share-shr0002',
    },
  ];
}

const encodeKey = (key: string) => key.split('/').map(encodeURIComponent).join('/');

function signedUrl(
  method: string,
  bucket: string,
  key: string,
  expires: number,
  extra = '',
): string {
  return (
    `${endpoint}/${encodeURIComponent(bucket)}/${encodeKey(key)}` +
    `?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=${expires}` +
    `&X-Amz-SignedHeaders=host&X-Amz-Signature=mock${method.toLowerCase()}${extra}`
  );
}

function checkExpiry(expiresIn: number | undefined, fallback: number): number {
  const seconds = expiresIn ?? fallback;
  if (!Number.isInteger(seconds) || seconds <= 0 || seconds > maxExpirySeconds) {
    throw problem(400, 'invalid_argument', 'The validity must be between 1 second and 7 days.');
  }
  return seconds;
}

/**
 * In-memory share links. The owner of a new link is the caller; listing,
 * revoking and revoking all follow the self-service rule of the server
 * (own links always, everybody's with admin:ManageShares).
 */
export function createMockShareService(
  ctx: MockContext,
  iam: MockIam,
  options: MockShareOptions = {},
): ShareService {
  const shares = new Map((options.shares ?? sampleShares()).map((s) => [s.id, clone(s)]));
  let counter = 0;

  const isAdmin = () => iam.allowed('admin:ManageShares');
  const checkOwner = (owner: string) => {
    if (owner !== iam.caller() && !isAdmin()) {
      throw problem(403, 'access_denied', 'Access denied: admin:ManageShares is not allowed.');
    }
  };
  const find = (id: string) => {
    const share = shares.get(id);
    if (!share) throw problem(404, 'not_found', `Share ${id} does not exist.`);
    checkOwner(share.owner);
    return share;
  };

  return {
    async list(filter) {
      await ctx.delay();
      const me = iam.caller();
      const owner = filter?.owner !== undefined && filter.owner !== '' ? filter.owner : undefined;
      if (owner !== undefined) checkOwner(owner);
      const ownerFilter = owner ?? (isAdmin() ? undefined : me);
      return [...shares.values()]
        .filter((s) => ownerFilter === undefined || s.owner === ownerFilter)
        .filter(
          (s) => filter?.bucket === undefined || filter.bucket === '' || s.bucket === filter.bucket,
        )
        .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
        .map(clone);
    },
    async get(id) {
      await ctx.delay();
      return clone(find(id));
    },
    async create(input) {
      await ctx.delay();
      const bucket = input.bucket ?? '';
      if (bucket === '' || input.key === '') {
        throw problem(400, 'invalid_argument', 'Bucket and key are required.');
      }
      const permission = input.permission ?? 'download';
      const action = permission === 'upload' ? 's3:PutObject' : 's3:GetObject';
      if (!iam.allowed(action, bucket, input.key)) {
        throw problem(403, 'access_denied', `Access denied: ${action} is not allowed.`);
      }
      const seconds = checkExpiry(input.expiresIn, maxExpirySeconds);
      counter += 1;
      const id = `mock${String(counter).padStart(4, '0')}`;
      const created = ctx.now();
      const prefix = input.prefix === true;
      const share: Share = {
        id,
        owner: iam.caller(),
        bucket,
        key: input.key,
        prefix,
        permission,
        ...(input.versionId !== undefined ? { versionId: input.versionId } : {}),
        ...(input.contentType !== undefined ? { contentType: input.contentType } : {}),
        ...(input.contentDisposition !== undefined
          ? { contentDisposition: input.contentDisposition }
          : {}),
        expiresAt: new Date(created.getTime() + seconds * 1000).toISOString(),
        createdAt: created.toISOString(),
        accessKey: `share-${id}`,
      };
      if (!(prefix && permission === 'upload')) {
        const disposition =
          input.contentDisposition !== undefined && input.contentDisposition !== ''
            ? `&response-content-disposition=${encodeURIComponent(input.contentDisposition)}`
            : '';
        share.url = prefix
          ? `${endpoint}/${encodeURIComponent(bucket)}?list-type=2&prefix=${encodeURIComponent(input.key)}&X-Amz-Expires=${seconds}&X-Amz-Signature=mock`
          : signedUrl(
              permission === 'upload' ? 'PUT' : 'GET',
              bucket,
              input.key,
              seconds,
              disposition,
            );
      }
      shares.set(id, share);
      return clone(share);
    },
    async revoke(id) {
      await ctx.delay();
      find(id);
      shares.delete(id);
    },
    async revokeAll(owner) {
      await ctx.delay();
      const target = owner !== undefined && owner !== '' ? owner : iam.caller();
      checkOwner(target);
      let revoked = 0;
      for (const [id, share] of shares) {
        if (share.owner === target) {
          shares.delete(id);
          revoked += 1;
        }
      }
      return revoked;
    },
    async presign(input) {
      await ctx.delay();
      const method = input.method ?? 'GET';
      const action =
        method === 'PUT'
          ? 's3:PutObject'
          : method === 'DELETE'
            ? 's3:DeleteObject'
            : 's3:GetObject';
      if (!iam.allowed(action, input.bucket, input.key)) {
        throw problem(403, 'access_denied', `Access denied: ${action} is not allowed.`);
      }
      const seconds = checkExpiry(input.expiresIn, maxExpirySeconds);
      const headers: Record<string, string> = { ...(input.headers ?? {}) };
      if (method === 'PUT' && input.contentType !== undefined && input.contentType !== '') {
        headers['Content-Type'] = input.contentType;
      }
      const result: PresignedUrl = {
        url: signedUrl(method, input.bucket, input.key, seconds),
        method,
        expiresAt: new Date(ctx.now().getTime() + seconds * 1000).toISOString(),
        headers,
      };
      return result;
    },
    async postPolicy(input) {
      await ctx.delay();
      if (!iam.allowed('s3:PutObject', input.bucket, input.key ?? input.keyPrefix ?? '')) {
        throw problem(403, 'access_denied', 'Access denied: s3:PutObject is not allowed.');
      }
      const seconds = checkExpiry(input.expiresIn, 3600);
      const expiresAt = new Date(ctx.now().getTime() + seconds * 1000).toISOString();
      const conditions: PostCondition[] = [{ operator: 'eq', key: 'bucket', value: input.bucket }];
      const fields: Record<string, string> = { ...(input.fields ?? {}) };
      if (input.keyPrefix !== undefined && input.keyPrefix !== '') {
        conditions.push({ operator: 'starts-with', key: 'key', value: input.keyPrefix });
        fields.key = `${input.keyPrefix}\${filename}`;
      } else {
        const key = input.key !== undefined && input.key !== '' ? input.key : '${filename}';
        conditions.push(
          key.includes('${filename}')
            ? { operator: 'starts-with', key: 'key', value: key.replace('${filename}', '') }
            : { operator: 'eq', key: 'key', value: key },
        );
        fields.key = key;
      }
      if (input.maxSize !== undefined && input.maxSize > 0) {
        conditions.push({
          operator: 'content-length-range',
          min: input.minSize ?? 0,
          max: input.maxSize,
        });
      }
      if (input.contentType !== undefined && input.contentType !== '') {
        conditions.push({ operator: 'eq', key: 'Content-Type', value: input.contentType });
        fields['Content-Type'] = input.contentType;
      } else if (input.contentTypePrefix !== undefined && input.contentTypePrefix !== '') {
        conditions.push({
          operator: 'starts-with',
          key: 'Content-Type',
          value: input.contentTypePrefix,
        });
      }
      if (input.successActionStatus !== undefined) {
        fields.success_action_status = String(input.successActionStatus);
      }
      const policyDocument = { expiration: expiresAt, conditions };
      Object.assign(fields, {
        'x-amz-algorithm': 'AWS4-HMAC-SHA256',
        'x-amz-credential': `${iam.caller()}/20261008/us-east-1/s3/aws4_request`,
        'x-amz-date': '20261008T000000Z',
        policy: btoa(JSON.stringify(policyDocument)),
        'x-amz-signature': 'mocksignature',
      });
      const result: PostPolicy = {
        url: `${endpoint}/${encodeURIComponent(input.bucket)}`,
        fields,
        expiresAt,
        conditions,
      };
      return result;
    },
  };
}
