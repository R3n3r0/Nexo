import type { MediaService } from '../interfaces';
import type { MediaConfiguration, MediaStatus } from '../types';
import type { HttpClient } from './client';

/** MediaService over /buckets/{bucket}/media and /buckets/{bucket}/objects/media/status. */
export function createHttpMediaService(client: HttpClient): MediaService {
  const path = (bucket: string, sub = '') =>
    `/buckets/${encodeURIComponent(bucket)}/media${sub === '' ? '' : `/${sub}`}`;

  return {
    get(bucket) {
      return client.request<MediaConfiguration>(path(bucket));
    },
    set(bucket, config) {
      return client.request<MediaConfiguration>(path(bucket), { method: 'PUT', body: config });
    },
    remove(bucket) {
      return client.request<void>(path(bucket), { method: 'DELETE' });
    },
    reprocess(bucket, request) {
      return client.request<void>(path(bucket, 'reprocess'), { method: 'POST', body: request });
    },
    status(bucket, key, versionId) {
      return client.request<MediaStatus>(
        `/buckets/${encodeURIComponent(bucket)}/objects/media/status`,
        { query: { key, versionId } },
      );
    },
  };
}
