import { createAuthEventEmitter } from '../events';
import type { Services } from '../interfaces';
import { createHttpBucketService } from './buckets';
import { createHttpClient, type HttpClientOptions } from './client';
import {
  createHttpAccessKeyService,
  createHttpGroupService,
  createHttpPolicyService,
  createHttpUserService,
} from './iam';
import { createHttpInfoService } from './info';
import { createHttpSessionService } from './session';

export type HttpServicesOptions = Omit<HttpClientOptions, 'onUnauthorized'>;

/**
 * Builds the service bundle backed by the Admin API. Every 401 answered by
 * the server (except on login) is broadcast through `authEvents`.
 */
export function createHttpServices(options: HttpServicesOptions = {}): Services {
  const authEvents = createAuthEventEmitter();
  const client = createHttpClient({
    ...options,
    onUnauthorized: () => {
      authEvents.emitUnauthorized();
    },
  });
  return {
    session: createHttpSessionService(client),
    info: createHttpInfoService(client),
    buckets: createHttpBucketService(client),
    users: createHttpUserService(client),
    groups: createHttpGroupService(client),
    policies: createHttpPolicyService(client),
    accessKeys: createHttpAccessKeyService(client),
    authEvents,
  };
}

export { createHttpClient, resolveUrl } from './client';
export type { HttpClient, HttpClientOptions, RequestOptions } from './client';
export { readCookie } from './csrf';
