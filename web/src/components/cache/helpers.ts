/**
 * Text helpers of the cache components: the scope of an invalidation, the
 * messages of the validation, the summary of the settings of a level.
 */
import type { TFunction } from 'i18next';

import { compactDuration, pruneSettings, type CacheIssue } from '@/lib/cache';
import type {
  KeyPattern,
  CacheDuration,
  CacheInvalidation,
  CacheInvalidationRequest,
  CacheSettings,
} from '@/services';

/** How often an operation in progress is read again. */
export const pollMs = 1_000;

/** A sentence naming what an invalidation covers. */
export function invalidationScope(
  t: TFunction,
  request: CacheInvalidation['request'] | CacheInvalidationRequest,
): string {
  const parts: string[] = [];
  const dist = 'distribution' in request ? request.distribution : undefined;
  if (request.all === true) {
    parts.push(
      dist === undefined || dist === ''
        ? t('cache.invalidations.scope.global')
        : t('cache.invalidations.scope.all'),
    );
  }
  if ((request.domain ?? '') !== '') {
    parts.push(t('cache.invalidations.scope.domain', { domain: request.domain }));
  }
  if ((request.paths ?? []).length > 0) {
    parts.push(t('cache.invalidations.scope.paths', { paths: (request.paths ?? []).join(' ') }));
  }
  for (const p of request.patterns ?? []) parts.push(typedPattern(p));
  if ((request.tags ?? []).length > 0) {
    parts.push(t('cache.invalidations.scope.tags', { tags: (request.tags ?? []).join(', ') }));
  }
  if (request.soft === true) parts.push(t('cache.invalidations.scope.soft'));
  return parts.join(' · ');
}

/** The translated message of a validation issue. */
export function issueMessage(t: TFunction, issue: CacheIssue): string {
  return t(`cache.issues.${issue.code}`, { field: issue.field, ...(issue.values ?? {}) });
}

/** The first issue of each field, translated. */
export function issuesByField(t: TFunction, issues: readonly CacheIssue[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const issue of issues) {
    if (!out.has(issue.field)) out.set(issue.field, issueMessage(t, issue));
  }
  return out;
}

/** A short sentence of what a level sets. */
export function settingsSummary(t: TFunction, s: CacheSettings): string {
  const parts: string[] = [];
  if (s.enabled === false) parts.push(t('cache.summary.disabled'));
  if (s.enabled === true) parts.push(t('cache.summary.enabled'));
  if (s.policy !== undefined && s.policy !== '') {
    parts.push(t('cache.summary.policy', { policy: s.policy }));
  }
  if (s.quota !== undefined) parts.push(t('cache.summary.quota', { quota: String(s.quota) }));
  if (s.ttl?.default !== undefined) {
    parts.push(t('cache.summary.ttlDefault', { ttl: compactDuration(s.ttl.default) }));
  }
  if (s.ttl?.min !== undefined) {
    parts.push(t('cache.summary.ttlMin', { ttl: compactDuration(s.ttl.min) }));
  }
  if (s.ttl?.max !== undefined) {
    parts.push(t('cache.summary.ttlMax', { ttl: compactDuration(s.ttl.max) }));
  }
  if (s.browserCacheControl !== undefined) {
    parts.push(
      s.browserCacheControl === ''
        ? t('cache.summary.objectCacheControl')
        : t('cache.summary.cacheControl', { value: s.browserCacheControl }),
    );
  }
  if ((s.purgeWebhooks ?? []).length > 0) {
    parts.push(t('cache.summary.webhooks', { webhooks: (s.purgeWebhooks ?? []).join(', ') }));
  }
  const rest = Object.keys(pruneSettings(s)).filter(
    (k) =>
      !['enabled', 'policy', 'quota', 'ttl', 'browserCacheControl', 'purgeWebhooks'].includes(k),
  ).length;
  if (rest > 0) parts.push(t('cache.summary.more', { n: rest }));
  return parts.length === 0 ? t('cache.summary.inherits') : parts.join(' · ');
}

/** A duration of the server in its compact form, or a dash. */
export function durationText(value: CacheDuration | undefined): string {
  const s = compactDuration(value);
  return s === '' ? '—' : s;
}

/** A pattern with its type, as the CLI prints it ("glob /assets/**"). */
export function typedPattern(p: KeyPattern): string {
  return `${p.type} ${p.pattern}${p.caseInsensitive === true ? ' (i)' : ''}`;
}
