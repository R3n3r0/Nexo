import type { LifecycleService } from '../interfaces';
import type { LifecycleApplyResult, LifecycleConfiguration } from '../types';
import type { HttpClient } from './client';

/** LifecycleService over /buckets/{bucket}/lifecycle. */
export function createHttpLifecycleService(client: HttpClient): LifecycleService {
  const path = (bucket: string, sub = '') =>
    `/buckets/${encodeURIComponent(bucket)}/lifecycle${sub === '' ? '' : `/${sub}`}`;

  return {
    get(bucket) {
      return client.request<LifecycleConfiguration>(path(bucket));
    },
    set(bucket, config) {
      return client.request<LifecycleConfiguration>(path(bucket), { method: 'PUT', body: config });
    },
    remove(bucket) {
      return client.request<void>(path(bucket), { method: 'DELETE' });
    },
    apply(bucket) {
      return client.request<LifecycleApplyResult>(path(bucket, 'apply'), { method: 'POST' });
    },
  };
}
