/**
 * Data transfer objects exchanged with the Admin API (/api/v1).
 *
 * Field names follow the JSON produced by the Go transport (camelCase).
 * Timestamps are RFC 3339 strings; sizes and counters are plain numbers.
 */

/** Deployment mode as reported by GET /info (see domain.Mode). */
export type DeploymentMode = 'snsd' | 'snmd' | 'distributed' | 'unknown';

/** Enabled/disabled state of a user, group or access key. */
export type CredentialStatus = 'enabled' | 'disabled';

/** Versioning state of a bucket. */
export type VersioningState = 'off' | 'enabled' | 'suspended';

/** Drive and node states (see domain.State*). */
export type ResourceState = 'online' | 'offline' | 'healing' | 'unknown';

/** The principal behind a console session. */
export interface SessionIdentity {
  accessKey: string;
  isRoot: boolean;
  groups: string[];
  policyNames: string[];
}

/** An authenticated console session (POST /auth/login, GET /auth/session). */
export interface Session {
  id: string;
  accessKey: string;
  identity: SessionIdentity;
  issuedAt: string;
  expiresAt: string;
  /** Token the browser echoes in the X-CSRF-Token header on mutating requests. */
  csrfToken: string;
}

export interface DiskInfo {
  endpoint: string;
  path: string;
  id: string;
  poolIdx: number;
  setIdx: number;
  diskIdx: number;
  state: ResourceState;
  total: number;
  free: number;
  used: number;
  healing: boolean;
  error?: string;
}

export interface NodeInfo {
  endpoint: string;
  state: ResourceState;
  version: string;
  commit: string;
  uptimeSeconds: number;
  disks: DiskInfo[];
}

export interface PoolInfo {
  index: number;
  sets: number;
  drivesPerSet: number;
  parity: number;
}

/** Aggregated server/cluster information (GET /info). */
export interface ServerInfo {
  version: string;
  commit: string;
  buildDate: string;
  mode: DeploymentMode;
  region: string;
  deploymentId: string;
  startedAt: string;
  uptimeSeconds: number;
  pools: PoolInfo[];
  nodes: NodeInfo[];
  buckets: number;
  objects: number;
  /** Logical size in bytes of all stored versions. */
  usage: number;
}

export interface Bucket {
  name: string;
  created: string;
  region: string;
  versioning: VersioningState;
  objectLockEnabled: boolean;
  objects: number;
  versions: number;
  size: number;
  /** Hard quota in bytes, 0 when unset. */
  quotaBytes: number;
  tags: Record<string, string>;
}

export interface CreateBucketInput {
  name: string;
  region?: string;
  objectLock?: boolean;
}

export interface RemoveBucketOptions {
  /** Delete the bucket even when it still contains objects. */
  force?: boolean;
}

export interface User {
  accessKey: string;
  status: CredentialStatus;
  policyNames: string[];
  groups: string[];
  createdAt: string;
}

export interface CreateUserInput {
  accessKey: string;
  secretKey: string;
  policyNames?: string[];
}

export interface Group {
  name: string;
  status: CredentialStatus;
  members: string[];
  policyNames: string[];
  createdAt: string;
}

export interface CreateGroupInput {
  name: string;
  members?: string[];
  policyNames?: string[];
}

/** An IAM policy with its JSON document (AWS format). */
export interface Policy {
  name: string;
  document: PolicyDocument;
  builtIn: boolean;
  updatedAt: string;
}

export interface PolicyDocument {
  Version: string;
  Statement: PolicyStatement[];
}

export interface PolicyStatement {
  Sid?: string;
  Effect: 'Allow' | 'Deny';
  Action: string | string[];
  Resource: string | string[];
  Condition?: Record<string, Record<string, string | string[]>>;
}

export interface SavePolicyInput {
  name: string;
  document: PolicyDocument;
}

export interface AccessKey {
  accessKey: string;
  parentUser: string;
  name: string;
  description: string;
  status: CredentialStatus;
  /** Absent for non-expiring keys. */
  expiration?: string;
  createdAt: string;
}

/** The access key just created: the only time the secret is visible. */
export interface CreatedAccessKey extends AccessKey {
  secretKey: string;
}

export interface CreateAccessKeyInput {
  parentUser?: string;
  name?: string;
  description?: string;
  expiration?: string;
  policy?: PolicyDocument;
}
