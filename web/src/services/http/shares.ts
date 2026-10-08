import type { ShareService } from '../interfaces';
import type { PostPolicy, PresignedUrl, RevokeAllResult, Share } from '../types';
import type { HttpClient } from './client';
import { collectPages, segment } from './pages';

/** ShareService over /shares, /presign and /post-policy. */
export function createHttpShareService(client: HttpClient): ShareService {
  return {
    list(filter) {
      return collectPages<Share>(client, '/shares', {
        owner: filter?.owner,
        bucket: filter?.bucket,
      });
    },
    get(id) {
      return client.request<Share>(`/shares/${segment(id)}`);
    },
    create(input) {
      return client.request<Share>('/shares', { method: 'POST', body: input });
    },
    revoke(id) {
      return client.request<void>(`/shares/${segment(id)}`, { method: 'DELETE' });
    },
    async revokeAll(owner) {
      const result = await client.request<RevokeAllResult>('/shares', {
        method: 'DELETE',
        query: { owner },
      });
      return result.revoked;
    },
    presign(input) {
      return client.request<PresignedUrl>('/presign', { method: 'POST', body: input });
    },
    postPolicy(input) {
      return client.request<PostPolicy>('/post-policy', { method: 'POST', body: input });
    },
  };
}
