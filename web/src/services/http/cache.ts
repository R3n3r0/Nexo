import type { CacheService } from '../interfaces';
import type {
  CacheEffectivePolicy,
  CacheGlobalSettings,
  CacheInspection,
  CacheInvalidation,
  CachePolicy,
  CachePolicyPreview,
  CacheStats,
  CacheWarmJob,
  CacheWebhook,
  CacheWebhookTestResult,
  DistributionCache,
} from '../types';
import { ifMatchHeader } from './accessRules';
import type { HttpClient } from './client';

interface Items<T> {
  items: T[];
}

/**
 * The fields of a policy the server ignores in requests (built-in flag,
 * stored version, update time): dropped so that the console sends the
 * policy as the user edited it.
 */
function policyBody(policy: CachePolicy): CachePolicy {
  const { builtin: _builtin, etag: _etag, updatedAt: _updatedAt, ...rest } = policy;
  return rest;
}

/**
 * CacheService over the cache routes of the Admin API (docs/11 §11.11):
 * /cache/policies, /cache/preview, /cache/settings, /cache/inspect,
 * /cache/stats, /cache/invalidations and the cache of a distribution
 * under /distributions/{id} (settings, effective policy, invalidations,
 * warm-ups, purge webhooks).
 */
export function createHttpCacheService(client: HttpClient): CacheService {
  const policyPath = (name: string) => `/cache/policies/${encodeURIComponent(name)}`;
  const distPath = (id: string, sub: string) => `/distributions/${encodeURIComponent(id)}/${sub}`;
  const invalidationsPath = (distribution: string) =>
    distribution === '' ? '/cache/invalidations' : distPath(distribution, 'invalidations');

  return {
    async listPolicies() {
      const page = await client.request<Items<CachePolicy>>('/cache/policies');
      return page.items;
    },
    getPolicy(name) {
      return client.request<CachePolicy>(policyPath(name));
    },
    createPolicy(policy) {
      return client.request<CachePolicy>('/cache/policies', {
        method: 'POST',
        body: policyBody(policy),
      });
    },
    updatePolicy(policy, ifMatch) {
      return client.request<CachePolicy>(policyPath(policy.name), {
        method: 'PUT',
        body: policyBody(policy),
        headers: ifMatchHeader(ifMatch),
      });
    },
    deletePolicy(name) {
      return client.request<void>(policyPath(name), { method: 'DELETE' });
    },
    previewPolicy(policy) {
      return client.request<CachePolicyPreview>('/cache/preview', {
        method: 'POST',
        body: policyBody(policy),
      });
    },
    getSettings() {
      return client.request<CacheGlobalSettings>('/cache/settings');
    },
    updateSettings(settings, ifMatch) {
      const { etag: _etag, updatedAt: _updatedAt, ...body } = settings;
      return client.request<CacheGlobalSettings>('/cache/settings', {
        method: 'PUT',
        body,
        headers: ifMatchHeader(ifMatch),
      });
    },
    getDistribution(distribution) {
      return client.request<DistributionCache>(distPath(distribution, 'cache'));
    },
    updateDistribution(config, ifMatch) {
      const { etag: _etag, updatedAt: _updatedAt, ...body } = config;
      return client.request<DistributionCache>(distPath(config.distribution ?? '', 'cache'), {
        method: 'PUT',
        body,
        headers: ifMatchHeader(ifMatch),
      });
    },
    effective(distribution, query = {}) {
      return client.request<CacheEffectivePolicy>(distPath(distribution, 'cache/effective'), {
        query: {
          host: query.host === '' ? undefined : query.host,
          path: query.path === '' ? undefined : query.path,
        },
      });
    },
    inspect(input) {
      return client.request<CacheInspection>('/cache/inspect', {
        query: {
          url: input.url,
          distribution:
            input.distribution === undefined || input.distribution === ''
              ? undefined
              : input.distribution,
          header: (input.headers ?? []).filter((h) => h.trim() !== ''),
        },
      });
    },
    stats(query = {}) {
      return client.request<CacheStats>('/cache/stats', {
        query: {
          distribution: query.distribution === '' ? undefined : query.distribution,
          host: query.host === '' ? undefined : query.host,
          top: query.top,
        },
      });
    },
    invalidate(distribution, request) {
      return client.request<CacheInvalidation>(invalidationsPath(distribution), {
        method: 'POST',
        body: request,
      });
    },
    async invalidations(distribution) {
      const page = await client.request<Items<CacheInvalidation>>(invalidationsPath(distribution));
      return page.items;
    },
    invalidation(distribution, id) {
      return client.request<CacheInvalidation>(
        `${invalidationsPath(distribution)}/${encodeURIComponent(id)}`,
      );
    },
    warm(distribution, request) {
      return client.request<CacheWarmJob>(distPath(distribution, 'cache/warm'), {
        method: 'POST',
        body: request,
      });
    },
    warmJob(distribution, id) {
      return client.request<CacheWarmJob>(
        distPath(distribution, `cache/warm/${encodeURIComponent(id)}`),
      );
    },
    async webhooks(distribution) {
      const page = await client.request<Items<CacheWebhook>>(
        distPath(distribution, 'cache/webhooks'),
      );
      return page.items;
    },
    addWebhook(distribution, webhook) {
      return client.request<CacheWebhook>(distPath(distribution, 'cache/webhooks'), {
        method: 'POST',
        body: webhook,
      });
    },
    removeWebhook(distribution, id) {
      return client.request<void>(
        distPath(distribution, `cache/webhooks/${encodeURIComponent(id)}`),
        { method: 'DELETE' },
      );
    },
    testWebhook(distribution, id) {
      return client.request<CacheWebhookTestResult>(
        distPath(distribution, `cache/webhooks/${encodeURIComponent(id)}/test`),
        { method: 'POST' },
      );
    },
  };
}
