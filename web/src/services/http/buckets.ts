import type { BucketService } from '../interfaces';
import type { Bucket } from '../types';
import type { HttpClient } from './client';

/** BucketService over /buckets and /buckets/{name}. */
export function createHttpBucketService(client: HttpClient): BucketService {
  return {
    list() {
      return client.request<Bucket[]>('/buckets');
    },
    get(name) {
      return client.request<Bucket>(`/buckets/${encodeURIComponent(name)}`);
    },
    create(input) {
      return client.request<Bucket>('/buckets', { method: 'POST', body: input });
    },
    remove(name, options) {
      return client.request<void>(`/buckets/${encodeURIComponent(name)}`, {
        method: 'DELETE',
        query: { force: options?.force },
      });
    },
  };
}
