import { createAuthEventEmitter } from '../events';
import type { Services } from '../interfaces';
import { createHttpAccessRuleService } from './accessRules';
import { createHttpBucketService } from './buckets';
import { createHttpClient, type HttpClientOptions } from './client';
import { createHttpInfoService } from './info';
import { lazyService } from './lazy';
import { createLazyHttpMonitoringService } from './lazyMonitoring';
import { createHttpObjectService } from './objects';
import { createHttpPermissionService } from './permissions';
import { createHttpSessionService } from './session';
import type { EventSourceFactory } from './stream';
import { createHttpUploadService } from './uploads';

export interface HttpServicesOptions extends Omit<HttpClientOptions, 'onUnauthorized'> {
  /** Opens the live streams (default: the browser EventSource). */
  eventSource?: EventSourceFactory;
}

/**
 * Builds the service bundle backed by the Admin API. Every 401 answered by
 * the server (except on login) is broadcast through `authEvents`.
 */
export function createHttpServices(options: HttpServicesOptions = {}): Services {
  const authEvents = createAuthEventEmitter();
  const { eventSource, ...clientOptions } = options;
  const client = createHttpClient({
    ...clientOptions,
    onUnauthorized: () => {
      authEvents.emitUnauthorized();
    },
  });
  return {
    session: createHttpSessionService(client),
    info: createHttpInfoService(client),
    buckets: createHttpBucketService(client),
    objects: createHttpObjectService(client),
    uploads: createHttpUploadService(client),
    permissions: createHttpPermissionService(client),
    // The services of the pages loaded on demand are loaded with them
    // (docs/06 §6.5: the initial bundle stays within its budget).
    heal: lazyService(async () => (await import('./heal')).createHttpHealService(client)),
    pools: lazyService(async () => (await import('./pools')).createHttpPoolsService(client)),
    lifecycle: lazyService(async () =>
      (await import('./lifecycle')).createHttpLifecycleService(client),
    ),
    notifications: lazyService(async () =>
      (await import('./notifications')).createHttpNotificationService(client),
    ),
    media: lazyService(async () => (await import('./media')).createHttpMediaService(client)),
    replication: lazyService(async () =>
      (await import('./replication')).createHttpReplicationService(client),
    ),
    siteReplication: lazyService(async () =>
      (await import('./siteReplication')).createHttpSiteReplicationService(client),
    ),
    tiers: lazyService(async () => (await import('./tiers')).createHttpTierService(client)),
    kms: lazyService(async () => (await import('./kms')).createHttpKmsService(client)),
    users: lazyService(async () => (await import('./iam')).createHttpUserService(client)),
    groups: lazyService(async () => (await import('./iam')).createHttpGroupService(client)),
    policies: lazyService(async () => (await import('./iam')).createHttpPolicyService(client)),
    accessKeys: lazyService(async () => (await import('./iam')).createHttpAccessKeyService(client)),
    shares: lazyService(async () => (await import('./shares')).createHttpShareService(client)),
    config: lazyService(async () => (await import('./config')).createHttpConfigService(client)),
    accessRules: createHttpAccessRuleService(client),
    distributions: lazyService(async () =>
      (await import('./distributions')).createHttpDistributionService(client),
    ),
    keyGroups: lazyService(async () =>
      (await import('./distributions')).createHttpKeyGroupService(client),
    ),
    certificates: lazyService(async () =>
      (await import('./distributions')).createHttpCertificateService(client),
    ),
    cache: lazyService(async () => (await import('./cache')).createHttpCacheService(client)),
    monitoring: createLazyHttpMonitoringService(client, eventSource ? { eventSource } : {}),
    authEvents,
  };
}

export { createHttpClient, resolveUrl } from './client';
export type {
  HttpClient,
  HttpClientOptions,
  RawRequestOptions,
  RequestOptions,
  UploadRequestOptions,
} from './client';
export { readCookie } from './csrf';
export type { EventSourceFactory, EventSourceLike } from './stream';
