import { isApiError } from '../errors';
import type { BucketService } from '../interfaces';
import type {
  Bucket,
  BucketAccess,
  BucketEncryption,
  BucketObjectLock,
  BucketQuota,
  BucketVersioning,
  TagSet,
} from '../types';
import type { HttpClient } from './client';
import { collectPages } from './pages';

/** Tags document of the Admin API ({tags}). */
interface TagsBody {
  tags: TagSet;
}

/** BucketService over /buckets, /buckets/{name} and its settings. */
export function createHttpBucketService(client: HttpClient): BucketService {
  const path = (name: string, sub = '') =>
    `/buckets/${encodeURIComponent(name)}${sub === '' ? '' : `/${sub}`}`;

  /**
   * Resolves to null when the configuration is missing (404 other than a
   * missing bucket): S3 answers NoSuchBucketPolicy, NoSuchEncryption...
   */
  async function optional<T>(promise: Promise<T>): Promise<T | null> {
    try {
      return await promise;
    } catch (error) {
      if (isApiError(error) && error.isNotFound && error.code !== 'bucket_not_found') {
        return null;
      }
      throw error;
    }
  }

  return {
    list() {
      return collectPages<Bucket>(client, '/buckets');
    },
    get(name) {
      return client.request<Bucket>(path(name));
    },
    create(input) {
      return client.request<Bucket>('/buckets', { method: 'POST', body: input });
    },
    remove(name, options) {
      return client.request<void>(path(name), {
        method: 'DELETE',
        query: { force: options?.force },
      });
    },
    async getVersioning(name) {
      const v = await client.request<BucketVersioning>(path(name, 'versioning'));
      return v.status;
    },
    async setVersioning(name, status) {
      const v = await client.request<BucketVersioning>(path(name, 'versioning'), {
        method: 'PUT',
        body: { status } satisfies BucketVersioning,
      });
      return v.status;
    },
    getQuota(name) {
      return client.request<BucketQuota>(path(name, 'quota'));
    },
    setQuota(name, sizeBytes) {
      const quota: BucketQuota = { type: sizeBytes > 0 ? 'hard' : '', size: sizeBytes };
      return client.request<BucketQuota>(path(name, 'quota'), { method: 'PUT', body: quota });
    },
    async getTags(name) {
      // A bucket without tags answers an empty set.
      const body = await client.request<TagsBody>(path(name, 'tags'));
      return body.tags;
    },
    async setTags(name, tags) {
      const body = await client.request<TagsBody>(path(name, 'tags'), {
        method: 'PUT',
        body: { tags } satisfies TagsBody,
      });
      return body.tags;
    },
    getObjectLock(name) {
      // A bucket without object lock answers {enabled: false}.
      return client.request<BucketObjectLock>(path(name, 'objectlock'));
    },
    setObjectLock(name, config) {
      return client.request<BucketObjectLock>(path(name, 'objectlock'), {
        method: 'PUT',
        body: config,
      });
    },
    getEncryption(name) {
      return optional(client.request<BucketEncryption>(path(name, 'encryption')));
    },
    async setEncryption(name, encryption) {
      if (encryption === null) {
        await client.request<void>(path(name, 'encryption'), { method: 'DELETE' });
        return null;
      }
      return client.request<BucketEncryption>(path(name, 'encryption'), {
        method: 'PUT',
        body: encryption,
      });
    },
    async getPolicy(name) {
      const response = await optional(client.send(path(name, 'policy')));
      if (response === null) {
        return null;
      }
      const text = await response.text();
      return prettyJson(text);
    },
    async setPolicy(name, text) {
      // The text travels verbatim so that malformed_policy errors are
      // located by line and column in what the user typed.
      await client.send(path(name, 'policy'), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        rawBody: text,
      });
    },
    deletePolicy(name) {
      return client.request<void>(path(name, 'policy'), { method: 'DELETE' });
    },
    getAccess(name) {
      return client.request<BucketAccess>(path(name, 'access'));
    },
    setAccess(name, input) {
      return client.request<BucketAccess>(path(name, 'access'), { method: 'PUT', body: input });
    },
    removeAccess(name, prefix) {
      return client.request<BucketAccess>(path(name, 'access'), {
        method: 'DELETE',
        query: { prefix },
      });
    },
  };
}

/** Indents a JSON document; returns the text unchanged when it is not JSON. */
function prettyJson(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text) as unknown, null, 2);
  } catch {
    return text;
  }
}
