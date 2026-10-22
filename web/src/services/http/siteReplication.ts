import type { SiteReplicationService } from '../interfaces';
import type { SiteReplicationInfo } from '../types';
import type { HttpClient } from './client';

/** SiteReplicationService over /site-replication. */
export function createHttpSiteReplicationService(client: HttpClient): SiteReplicationService {
  return {
    info() {
      return client.request<SiteReplicationInfo>('/site-replication');
    },
    add(sites) {
      return client.request<SiteReplicationInfo>('/site-replication', {
        method: 'POST',
        body: { sites },
      });
    },
    remove() {
      return client.request<void>('/site-replication', { method: 'DELETE' });
    },
    sync() {
      return client.request<void>('/site-replication/sync', { method: 'POST' });
    },
  };
}
