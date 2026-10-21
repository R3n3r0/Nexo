import { demoCredentials } from '../demo';
import { createAuthEventEmitter } from '../events';
import type { Services } from '../interfaces';
import { createMockAccessRuleService, type MockAccessRuleOptions } from './accessRules';
import { createMockBucketService, sampleBuckets, type MockBucketOptions } from './buckets';
import { createMockCacheService, type MockCacheOptions } from './cache';
import { createMockConfigService, type MockConfigOptions } from './config';
import {
  createMockCertificateService,
  createMockDistributionService,
  createMockDistributionState,
  createMockKeyGroupService,
  type MockDistributionOptions,
} from './distributions';
import { createMockHealService, type MockHealOptions } from './heal';
import { createMockIam, type MockIamOptions } from './iam';
import { createMockInfoService, sampleServerInfo, type MockInfoOptions } from './info';
import { createMockKmsService } from './kms';
import { createMockLifecycleService } from './lifecycle';
import { createMockMediaService } from './media';
import {
  createMockMonitoringService,
  type MockMonitoringOptions,
  type MockMonitoringService,
} from './monitoring';
import { createMockNotificationService, type MockNotificationOptions } from './notifications';
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
    MockHealOptions,
    MockBucketOptions,
    MockStorageOptions,
    MockIamOptions,
    MockShareOptions,
    MockConfigOptions,
    MockNotificationOptions,
    MockAccessRuleOptions,
    MockDistributionOptions,
    MockCacheOptions,
    MockMonitoringOptions {
  /** Accepted logins; defaults to the demo credentials. */
  accounts?: Record<string, string>;
  /** Start authenticated as this account (tests of protected pages). */
  initialAccessKey?: string;
  /** Identity providers offered by the login page (default: none). */
  authProviders?: MockSessionOptions['providers'];
  /** LDAP users of the mock directory: username -> password. */
  ldapAccounts?: Record<string, string>;
}

/** The mock bundle exposes the session mock so tests can expire the session. */
export interface MockServices extends Services {
  readonly session: MockSessionService;
  /** The monitoring mock drives the live streams of the tests. */
  readonly monitoring: MockMonitoringService;
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
    ...(options.authProviders !== undefined ? { providers: options.authProviders } : {}),
    ...(options.ldapAccounts !== undefined ? { ldapAccounts: options.ldapAccounts } : {}),
  };
  const session = createMockSessionService(ctx, sessionOptions);
  const objects = createMockObjectService(storage);
  const dist = createMockDistributionState(
    ctx,
    options,
    (name) => storage.buckets.has(name),
    options.buckets === undefined,
    (action) => iam.allowed(action),
  );
  const serverInfo =
    options.info ??
    sampleServerInfo({
      kms: { ...storage.kms.status, keys: storage.kms.keys.map((k) => k.id) },
    });
  return {
    session,
    info: createMockInfoService(ctx, { ...options, info: serverInfo }, storage),
    heal: createMockHealService(ctx, () => serverInfo, options),
    buckets: createMockBucketService(storage),
    lifecycle: createMockLifecycleService(storage),
    notifications: createMockNotificationService(storage, options),
    media: createMockMediaService(storage),
    kms: createMockKmsService(storage),
    objects,
    uploads: createMockUploadService(storage),
    users: iam.users,
    groups: iam.groups,
    policies: iam.policies,
    accessKeys: iam.accessKeys,
    shares: createMockShareService(ctx, iam, options, objects),
    config: createMockConfigService(ctx, iam, options),
    permissions: iam.permissions,
    accessRules: createMockAccessRuleService(
      storage,
      {
        current: () => session.currentAccessKey() ?? undefined,
        rootAccessKey: Object.keys(accounts)[0] ?? demoCredentials.accessKey,
        allowed: (action, bucket) => iam.allowed(action, bucket),
      },
      options,
    ),
    distributions: createMockDistributionService(ctx, dist),
    keyGroups: createMockKeyGroupService(ctx, dist),
    certificates: createMockCertificateService(ctx, dist),
    cache: createMockCacheService(
      ctx,
      {
        dist,
        actor: () => session.currentAccessKey() ?? '',
        keysOf: (bucket) => {
          const state = storage.buckets.get(bucket);
          if (state === undefined) return [];
          const out: { key: string; size: number }[] = [];
          for (const [key, versions] of state.keys) {
            const latest = versions[0];
            if (latest !== undefined && !latest.isDeleteMarker)
              out.push({ key, size: latest.size });
          }
          return out;
        },
      },
      options,
      options.buckets === undefined,
    ),
    monitoring: createMockMonitoringService(
      ctx,
      { info: () => serverInfo, allowed: (action) => iam.allowed(action) },
      options,
    ),
    authEvents,
  };
}

export { isValidBucketName, sampleBuckets } from './buckets';
export { sampleConfigSchema } from './config';
export { sampleCacheConfig, sampleCacheStats } from './cache';
export { sampleDistributions } from './distributions';
export { sampleAccessKeys, sampleGroups, samplePolicies, sampleUsers } from './iam';
export { maxExpirySeconds, sampleShares } from './shares';
export { sampleClusterInfo, sampleServerInfo } from './info';
export { sampleAuditEntries, sampleMetrics, type MockMonitoringService } from './monitoring';
export { sampleNotifyTargets, sampleObjects, type MockObjectSeed } from './storage';
export type { MockSessionService } from './session';
