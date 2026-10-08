import { createAuthEventEmitter } from '../events';
import type { Services } from '../interfaces';
import { createMockBucketService, type MockBucketOptions } from './buckets';
import {
  createMockAccessKeyService,
  createMockGroupService,
  createMockPolicyService,
  createMockUserService,
  type MockIamOptions,
} from './iam';
import { createMockInfoService, type MockInfoOptions } from './info';
import {
  createMockSessionService,
  type MockSessionOptions,
  type MockSessionService,
} from './session';
import { createMockContext, type MockBehaviour } from './support';

/** Credentials accepted by the demo mode. */
export const demoCredentials = { accessKey: 'nexoadmin', secretKey: 'nexoadmin' } as const;

export interface MockServicesOptions
  extends MockBehaviour, MockInfoOptions, MockBucketOptions, MockIamOptions {
  /** Accepted logins; defaults to the demo credentials. */
  accounts?: Record<string, string>;
  /** Start authenticated as this account (tests of protected pages). */
  initialAccessKey?: string;
}

/** The mock bundle exposes the session mock so tests can expire the session. */
export interface MockServices extends Services {
  readonly session: MockSessionService;
}

/**
 * Builds a fully in-memory service bundle. Used by unit tests and by the
 * demo mode (VITE_DEMO=1), where no backend is available.
 */
export function createMockServices(options: MockServicesOptions = {}): MockServices {
  const ctx = createMockContext(options);
  const authEvents = createAuthEventEmitter();
  const sessionOptions: MockSessionOptions = {
    accounts: options.accounts ?? { [demoCredentials.accessKey]: demoCredentials.secretKey },
    authEvents,
    ...(options.initialAccessKey !== undefined
      ? { initialAccessKey: options.initialAccessKey }
      : {}),
  };
  return {
    session: createMockSessionService(ctx, sessionOptions),
    info: createMockInfoService(ctx, options),
    buckets: createMockBucketService(ctx, options),
    users: createMockUserService(ctx, options),
    groups: createMockGroupService(ctx, options),
    policies: createMockPolicyService(ctx, options),
    accessKeys: createMockAccessKeyService(ctx, options),
    authEvents,
  };
}

export { isValidBucketName, sampleBuckets } from './buckets';
export { sampleAccessKeys, sampleGroups, samplePolicies, sampleUsers } from './iam';
export { sampleServerInfo } from './info';
export type { MockSessionService } from './session';
