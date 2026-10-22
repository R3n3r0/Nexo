import type { ReplicationService } from '../interfaces';
import type {
  ReplicationConfiguration,
  ReplicationResync,
  ReplicationStatus,
  ReplicationTarget,
} from '../types';
import type { HttpClient } from './client';

/** ReplicationService over /buckets/{bucket}/replication and its sub-resources. */
export function createHttpReplicationService(client: HttpClient): ReplicationService {
  const path = (bucket: string, sub = '') =>
    `/buckets/${encodeURIComponent(bucket)}/replication${sub === '' ? '' : `/${sub}`}`;
  return {
    get(bucket) {
      return client.request<ReplicationConfiguration>(path(bucket));
    },
    set(bucket, config) {
      return client.request<ReplicationConfiguration>(path(bucket), {
        method: 'PUT',
        body: config,
      });
    },
    remove(bucket) {
      return client.request<void>(path(bucket), { method: 'DELETE' });
    },
    async targets(bucket) {
      const out = await client.request<{ targets: ReplicationTarget[] }>(path(bucket, 'targets'));
      return out.targets;
    },
    setTarget(bucket, target) {
      return client.request<ReplicationTarget>(path(bucket, 'targets'), {
        method: 'POST',
        body: target,
      });
    },
    removeTarget(bucket, arn) {
      return client.request<void>(path(bucket, 'targets'), {
        method: 'DELETE',
        query: { arn },
      });
    },
    status(bucket) {
      return client.request<ReplicationStatus>(path(bucket, 'status'));
    },
    resync(bucket, target) {
      return client.request<ReplicationResync>(path(bucket, 'resync'), {
        method: 'POST',
        body: { target },
      });
    },
  };
}
