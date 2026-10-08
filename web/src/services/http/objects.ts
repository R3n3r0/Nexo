import { isApiError } from '../errors';
import type { ObjectService } from '../interfaces';
import type {
  DeleteObjectsResult,
  ObjectDetails,
  ObjectEntry,
  ObjectList,
  RenameResult,
  TagSet,
  VersionList,
} from '../types';
import type { HttpClient } from './client';

/** ObjectService over /buckets/{bucket}/objects and its sub-resources. */
export function createHttpObjectService(client: HttpClient): ObjectService {
  const path = (bucket: string, sub = '') =>
    `/buckets/${encodeURIComponent(bucket)}/objects${sub === '' ? '' : `/${sub}`}`;

  return {
    list(bucket, query = {}) {
      return client.request<ObjectList>(path(bucket), {
        query: {
          prefix: query.prefix,
          delimiter: query.delimiter,
          cursor: query.cursor,
          limit: query.limit,
        },
      });
    },
    stat(bucket, key, versionId) {
      return client.request<ObjectDetails>(path(bucket, 'stat'), { query: { key, versionId } });
    },
    versions(bucket, key, query = {}) {
      return client.request<VersionList>(path(bucket, 'versions'), {
        query: { key, cursor: query.cursor, limit: query.limit },
      });
    },
    createFolder(bucket, prefix) {
      return client.request<ObjectEntry>(path(bucket, 'folder'), {
        method: 'POST',
        body: { prefix },
      });
    },
    remove(bucket, input) {
      return client.request<DeleteObjectsResult>(path(bucket), { method: 'DELETE', body: input });
    },
    rename(bucket, input) {
      return client.request<RenameResult>(path(bucket, 'rename'), { method: 'POST', body: input });
    },
    restoreVersion(bucket, key, versionId) {
      return client.request<ObjectEntry>(path(bucket, 'restore'), {
        method: 'POST',
        query: { key, versionId },
      });
    },
    async setTags(bucket, key, tags, versionId) {
      const body = await client.request<{ tags: TagSet }>(path(bucket, 'tags'), {
        method: 'PUT',
        query: { key, versionId },
        body: { tags },
      });
      return body.tags;
    },
    downloadUrl(bucket, key, versionId) {
      return client.url(path(bucket, 'download'), { key, versionId });
    },
    previewUrl(bucket, key, versionId) {
      return client.url(path(bucket, 'preview'), { key, versionId });
    },
    async previewText(bucket, key, { maxBytes, versionId, signal }) {
      let response: Response;
      try {
        response = await client.send(path(bucket, 'preview'), {
          query: { key, versionId },
          headers: { Accept: '*/*', Range: `bytes=0-${Math.max(0, maxBytes - 1)}` },
          ...(signal ? { signal } : {}),
        });
      } catch (error) {
        // An empty object has no byte 0: the range is not satisfiable.
        if (isApiError(error) && error.status === 416) {
          return { text: '', truncated: false, size: 0, contentType: 'text/plain' };
        }
        throw error;
      }
      const contentType = response.headers.get('content-type') ?? 'application/octet-stream';
      const size = totalSize(response);
      const bytes = new Uint8Array(await response.arrayBuffer());
      const head = bytes.byteLength > maxBytes ? bytes.subarray(0, maxBytes) : bytes;
      const truncated =
        bytes.byteLength > maxBytes || (size !== undefined && size > head.byteLength);
      return {
        text: new TextDecoder('utf-8').decode(head),
        truncated,
        ...(size !== undefined ? { size } : {}),
        contentType,
      };
    },
    downloadZip(bucket, input, signal) {
      // A form navigation: the browser writes the archive to disk as the
      // server streams it, never holding it in memory.
      return client.download(
        path(bucket, 'download-zip'),
        { request: JSON.stringify(input) },
        signal ? { signal } : {},
      );
    },
  };
}

/** Size of the whole object from Content-Range (206) or Content-Length (200). */
function totalSize(response: Response): number | undefined {
  const range = response.headers.get('content-range');
  if (range !== null) {
    const total = /\/(\d+)$/.exec(range)?.[1];
    return total !== undefined ? Number(total) : undefined;
  }
  const length = response.headers.get('content-length');
  return length !== null && response.status === 200 ? Number(length) : undefined;
}
