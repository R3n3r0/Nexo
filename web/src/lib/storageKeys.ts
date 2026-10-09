/**
 * TanStack Query keys of the storage pages. Mutations invalidate by prefix:
 * ['buckets'] refreshes every bucket query, ['objects', bucket] every
 * listing of a bucket. The permissions of GET /me/permissions are cached
 * under iamKeys.permissionsOf (lib/iamQueries.ts), shared with the IAM pages.
 */
export const storageKeys = {
  buckets: ['buckets'] as const,
  bucket: (name: string) => ['buckets', name] as const,
  bucketSetting: (name: string, setting: string) => ['buckets', name, setting] as const,
  usage: ['info', 'usage'] as const,
  objects: (bucket: string) => ['objects', bucket] as const,
  listing: (bucket: string, prefix: string) => ['objects', bucket, 'list', prefix] as const,
  object: (bucket: string, key: string, versionId?: string) =>
    ['objects', bucket, 'stat', key, versionId ?? ''] as const,
  versions: (bucket: string, key: string) => ['objects', bucket, 'versions', key] as const,
  preview: (bucket: string, key: string, versionId?: string) =>
    ['objects', bucket, 'preview', key, versionId ?? ''] as const,
  /** Object Lock state and attributes of a version (retention, legal hold, attributes). */
  objectLock: (bucket: string, key: string, versionId?: string) =>
    ['objects', bucket, 'lock', key, versionId ?? ''] as const,
  attributes: (bucket: string, key: string, versionId?: string) =>
    ['objects', bucket, 'attributes', key, versionId ?? ''] as const,
  /** Notification targets with their state (GET /notify/targets). */
  notifyTargets: ['notify', 'targets'] as const,
  kmsStatus: ['kms', 'status'] as const,
  kmsKeys: ['kms', 'keys'] as const,
};
