import type { KmsService } from '../interfaces';
import type { KmsKey, KmsStatus } from '../types';
import type { HttpClient } from './client';

/** KmsService over /kms/status and /kms/keys. */
export function createHttpKmsService(client: HttpClient): KmsService {
  return {
    status() {
      return client.request<KmsStatus>('/kms/status');
    },
    async keys(prefix) {
      const body = await client.request<{ items: KmsKey[] }>('/kms/keys', {
        query: { prefix: prefix !== undefined && prefix !== '' ? prefix : undefined },
      });
      return body.items;
    },
    createKey(id) {
      return client.request<KmsKey>('/kms/keys', { method: 'POST', body: { id } });
    },
  };
}
