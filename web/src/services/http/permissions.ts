import type { PermissionService } from '../interfaces';
import type { Permissions } from '../types';
import type { HttpClient } from './client';

/** PermissionService over GET /me/permissions. */
export function createHttpPermissionService(client: HttpClient): PermissionService {
  return {
    get(query) {
      return client.request<Permissions>('/me/permissions', {
        query: { user: query?.user, bucket: query?.bucket, prefix: query?.prefix },
      });
    },
  };
}
