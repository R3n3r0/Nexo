import type { AccessRuleService } from '../interfaces';
import type {
  AccessRules,
  AccessRulesPublicStatus,
  AccessRulesTestResult,
  BlockPublic,
  BucketAccessRule,
} from '../types';
import type { HttpClient } from './client';

/** The If-Match header of a conditional change: the etag, quoted. */
export function ifMatchHeader(etag: string | undefined): Record<string, string> {
  return etag === undefined || etag === '' ? {} : { 'If-Match': `"${etag}"` };
}

/**
 * AccessRuleService over /buckets/{bucket}/access-rules, its tester and
 * presets, and the block public access of the buckets and of the cluster
 * (/access/block-public).
 */
export function createHttpAccessRuleService(client: HttpClient): AccessRuleService {
  const path = (bucket: string, sub = '') =>
    `/buckets/${encodeURIComponent(bucket)}/${sub === '' ? 'access-rules' : sub}`;
  const blockPublicPath = (bucket?: string) =>
    bucket === undefined || bucket === '' ? '/access/block-public' : path(bucket, 'block-public');

  return {
    get(bucket) {
      return client.request<AccessRules>(path(bucket));
    },
    set(bucket, rules: BucketAccessRule[], ifMatch) {
      return client.request<AccessRules>(path(bucket), {
        method: 'PUT',
        body: { rules },
        headers: ifMatchHeader(ifMatch),
      });
    },
    test(bucket, input) {
      return client.request<AccessRulesTestResult>(path(bucket, 'access-rules/test'), {
        method: 'POST',
        body: input,
      });
    },
    publicStatus(bucket) {
      return client.request<AccessRulesPublicStatus>(path(bucket, 'access-rules/public'));
    },
    applyPreset(bucket, input, ifMatch) {
      return client.request<AccessRules>(path(bucket, 'access-rules/public'), {
        method: 'PUT',
        body: input,
        headers: ifMatchHeader(ifMatch),
      });
    },
    getBlockPublic(bucket) {
      return client.request<BlockPublic>(blockPublicPath(bucket));
    },
    setBlockPublic(input, bucket) {
      return client.request<BlockPublic>(blockPublicPath(bucket), { method: 'PUT', body: input });
    },
  };
}
