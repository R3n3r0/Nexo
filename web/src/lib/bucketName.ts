/** Why a bucket name is not valid; each issue has a message under buckets.nameIssues.*. */
export type BucketNameIssue =
  'required' | 'length' | 'characters' | 'edges' | 'dots' | 'ipAddress' | 'reserved';

/**
 * Validates a bucket name like domain.ValidateBucketName on the server
 * (DNS-compliant S3 rules), returning the first issue or null.
 */
export function bucketNameIssue(name: string): BucketNameIssue | null {
  if (name === '') return 'required';
  if (name.length < 3 || name.length > 63) return 'length';
  if (!/^[a-z0-9.-]+$/.test(name)) return 'characters';
  if (!/^[a-z0-9]/.test(name) || !/[a-z0-9]$/.test(name)) return 'edges';
  if (name.includes('..') || name.includes('.-') || name.includes('-.')) return 'dots';
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(name)) return 'ipAddress';
  if (
    name === 'nexo' ||
    name.startsWith('xn--') ||
    name.startsWith('sthree-') ||
    name.endsWith('-s3alias') ||
    name.endsWith('--ol-s3')
  ) {
    return 'reserved';
  }
  return null;
}
