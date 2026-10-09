import type { NotificationService } from '../interfaces';
import type { BucketNotifications, NotifyTarget, NotifyTargetTest } from '../types';
import type { HttpClient } from './client';

/** NotificationService over /buckets/{bucket}/events and /notify/targets. */
export function createHttpNotificationService(client: HttpClient): NotificationService {
  const path = (bucket: string) => `/buckets/${encodeURIComponent(bucket)}/events`;

  return {
    get(bucket) {
      return client.request<BucketNotifications>(path(bucket));
    },
    set(bucket, config) {
      return client.request<BucketNotifications>(path(bucket), { method: 'PUT', body: config });
    },
    remove(bucket) {
      return client.request<void>(path(bucket), { method: 'DELETE' });
    },
    async targets() {
      const body = await client.request<{ items: NotifyTarget[] }>('/notify/targets');
      return body.items;
    },
    test(target) {
      return client.request<NotifyTargetTest>(
        `/notify/targets/${encodeURIComponent(target)}/test`,
        { method: 'POST' },
      );
    },
  };
}
