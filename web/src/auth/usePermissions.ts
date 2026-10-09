import { useQuery } from '@tanstack/react-query';
import { useCallback } from 'react';

import { iamKeys } from '@/lib/iamQueries';
import { useServices, type Permissions, type PermissionsQuery } from '@/services';

import { useSession } from './SessionProvider';

/** Policy actions checked by the storage pages (catalog of GET /me/permissions). */
export const StorageActions = {
  createBucket: 's3:CreateBucket',
  deleteBucket: 's3:DeleteBucket',
  forceDeleteBucket: 's3:ForceDeleteBucket',
  listBucket: 's3:ListBucket',
  listVersions: 's3:ListBucketVersions',
  getBucketPolicy: 's3:GetBucketPolicy',
  putBucketPolicy: 's3:PutBucketPolicy',
  deleteBucketPolicy: 's3:DeleteBucketPolicy',
  getVersioning: 's3:GetBucketVersioning',
  putVersioning: 's3:PutBucketVersioning',
  getTagging: 's3:GetBucketTagging',
  putTagging: 's3:PutBucketTagging',
  getQuota: 'admin:GetBucketQuota',
  putEncryption: 's3:PutEncryptionConfiguration',
  putObjectLock: 's3:PutBucketObjectLockConfiguration',
  setQuota: 'admin:SetBucketQuota',
  getObject: 's3:GetObject',
  getObjectVersion: 's3:GetObjectVersion',
  putObject: 's3:PutObject',
  deleteObject: 's3:DeleteObject',
  deleteObjectVersion: 's3:DeleteObjectVersion',
  putObjectTagging: 's3:PutObjectTagging',
  getLifecycle: 's3:GetLifecycleConfiguration',
  putLifecycle: 's3:PutLifecycleConfiguration',
  applyLifecycle: 'admin:ApplyLifecycle',
  getNotification: 's3:GetBucketNotification',
  putNotification: 's3:PutBucketNotification',
  getCors: 's3:GetBucketCORS',
  putCors: 's3:PutBucketCORS',
  getEncryption: 's3:GetEncryptionConfiguration',
  getObjectLock: 's3:GetBucketObjectLockConfiguration',
  getRetention: 's3:GetObjectRetention',
  putRetention: 's3:PutObjectRetention',
  bypassGovernance: 's3:BypassGovernanceRetention',
  getLegalHold: 's3:GetObjectLegalHold',
  putLegalHold: 's3:PutObjectLegalHold',
  getAttributes: 's3:GetObjectAttributes',
  configUpdate: 'admin:ConfigUpdate',
  kmsStatus: 'admin:KMSKeyStatus',
  kmsCreateKey: 'admin:KMSCreateKey',
} as const;

export interface PermissionCheck {
  /** True until the first answer of GET /me/permissions. */
  loading: boolean;
  /** True once the permissions are known (the opposite of loading, false on error). */
  ready: boolean;
  /** The last answer, if any. */
  permissions: Permissions | undefined;
  /**
   * True when the action is allowed. Unknown permissions (loading, error)
   * count as denied: the UI never offers an action it cannot vouch for,
   * and the server authorizes every request anyway.
   */
  can: (action: string) => boolean;
}

export interface UsePermissionsOptions {
  /** Set false to skip the request (e.g. a closed dialog). */
  enabled?: boolean;
}

/**
 * Effective permissions of the signed-in user (or of query.user), from
 * GET /me/permissions, globally or on query.bucket/query.prefix. Pages use
 * them to hide or disable the actions the user may not perform (D-UX-02,
 * TD-U-09).
 */
export function usePermissions(
  query: PermissionsQuery = {},
  options: UsePermissionsOptions = {},
): PermissionCheck {
  const { permissions: service } = useServices();
  const { status } = useSession();
  const result = useQuery({
    queryKey: iamKeys.permissionsOf(query),
    queryFn: () =>
      service.get({
        ...(query.user !== undefined && query.user !== '' ? { user: query.user } : {}),
        ...(query.bucket !== undefined && query.bucket !== '' ? { bucket: query.bucket } : {}),
        ...(query.prefix !== undefined && query.prefix !== '' ? { prefix: query.prefix } : {}),
      }),
    enabled: status === 'authenticated' && options.enabled !== false,
    staleTime: 60_000,
  });
  const data = result.data;
  const can = useCallback(
    (action: string) => {
      if (data === undefined) {
        return false;
      }
      return data.isRoot || data.allowed.includes(action);
    },
    [data],
  );
  return {
    loading: result.isPending,
    ready: data !== undefined,
    permissions: data,
    can,
  };
}
