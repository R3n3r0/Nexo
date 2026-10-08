import { demoCredentials } from '../demo';
import { createAuthEventEmitter } from '../events';
import type { Services } from '../interfaces';
import { createMockBucketService, sampleBuckets, type MockBucketOptions } from './buckets';
import { createMockConfigService, type MockConfigOptions } from './config';
import { createMockIam, type MockIamOptions } from './iam';
import { createMockInfoService, type MockInfoOptions } from './info';
import { createMockObjectService } from './objects';
import {
  createMockSessionService,
  type MockSessionOptions,
  type MockSessionService,
} from './session';
import { createMockShareService, type MockShareOptions } from './shares';
import { createMockStorage, type MockStorageOptions } from './storage';
import { createMockContext, type MockBehaviour } from './support';
import { createMockUploadService } from './uploads';

export { demoCredentials } from '../demo';

export interface MockServicesOptions
  extends
    MockBehaviour,
    MockInfoOptions,
    MockBucketOptions,
    MockStorageOptions,
    MockIamOptions,
    MockShareOptions,
    MockConfigOptions {
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
  const storage = createMockStorage(ctx, options, { buckets: sampleBuckets });
  const accounts = options.accounts ?? {
    [demoCredentials.accessKey]: demoCredentials.secretKey,
  };
  // The IAM mocks authorize the signed-in principal; the first account is root.
  const iam = createMockIam(ctx, options, {
    current: () => session.currentAccessKey(),
    rootAccessKey: Object.keys(accounts)[0] ?? demoCredentials.accessKey,
  });
  const sessionOptions: MockSessionOptions = {
    accounts,
    authEvents,
    identityOf: (accessKey) => iam.identity(accessKey),
    verify: (accessKey, secretKey) => iam.verifySecret(accessKey, secretKey),
    ...(options.initialAccessKey !== undefined
      ? { initialAccessKey: options.initialAccessKey }
      : {}),
  };
  const session = createMockSessionService(ctx, sessionOptions);
  const objects = createMockObjectService(storage);
  return {
    session,
    info: createMockInfoService(ctx, options, storage),
    buckets: createMockBucketService(storage),
    objects,
    uploads: createMockUploadService(storage),
    users: iam.users,
    groups: iam.groups,
    policies: iam.policies,
    accessKeys: iam.accessKeys,
    shares: createMockShareService(ctx, iam, options, objects),
    config: createMockConfigService(ctx, iam, options),
    permissions: iam.permissions,
    authEvents,
  };
}

export { isValidBucketName, sampleBuckets } from './buckets';
export { sampleConfigSchema } from './config';
export { sampleAccessKeys, sampleGroups, samplePolicies, sampleUsers } from './iam';
export { maxExpirySeconds, sampleShares } from './shares';
export { sampleServerInfo } from './info';
export { sampleObjects, type MockObjectSeed } from './storage';
export type { MockSessionService } from './session';
