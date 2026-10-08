/**
 * Data transfer objects exchanged with the Admin API (/api/v1).
 *
 * Every wire type is derived from the OpenAPI contract (api/openapi.yaml)
 * through the generated `http/schema.d.ts` (`npm run gen:api`), so that the
 * console and the Go transport cannot drift apart: a change to the contract
 * fails `npm run typecheck` here. Field names are camelCase, timestamps are
 * RFC 3339 strings and sizes/counters are plain numbers.
 *
 * The handful of types at the end are console-side inputs (not wire
 * documents) composed from the schema types.
 */
import type { components } from './http/schema';

type Schemas = components['schemas'];

/** Deployment mode as reported by GET /info (see domain.Mode). */
export type DeploymentMode = Schemas['DeploymentMode'];

/** Enabled/disabled state of a user, group or access key. */
export type CredentialStatus = Schemas['CredentialStatus'];

/** Versioning state of a bucket. */
export type VersioningState = Schemas['VersioningState'];

/** Drive and node states (see domain.State*). */
export type ResourceState = Schemas['ResourceState'];

/** The principal behind a console session. */
export type SessionIdentity = Schemas['SessionIdentity'];

/** An authenticated console session (POST /auth/login, GET /auth/session). */
export type Session = Schemas['Session'];

export type DiskInfo = Schemas['DiskInfo'];
export type NodeInfo = Schemas['NodeInfo'];
export type PoolInfo = Schemas['PoolInfo'];

/** Aggregated server/cluster information (GET /info). */
export type ServerInfo = Schemas['ServerInfo'];

export type Bucket = Schemas['Bucket'];
export type CreateBucketInput = Schemas['CreateBucketRequest'];

export type User = Schemas['User'];
export type CreateUserInput = Schemas['CreateUserRequest'];

export type Group = Schemas['Group'];
export type CreateGroupInput = Schemas['CreateGroupRequest'];

/** An IAM policy with its JSON document (AWS format). */
export type Policy = Schemas['Policy'];
export type PolicyDocument = Schemas['PolicyDocument'];
export type PolicyStatement = Schemas['PolicyStatement'];

export type AccessKey = Schemas['AccessKey'];
/** The access key just created: the only time the secret is visible. */
export type CreatedAccessKey = Schemas['CreatedAccessKey'];
export type CreateAccessKeyInput = Schemas['CreateAccessKeyRequest'];

/* Console-side inputs composed from the wire types. */

export interface RemoveBucketOptions {
  /** Delete the bucket even when it still contains objects. */
  force?: boolean;
}

/** PUT /policies/{name}: the name travels in the path, the document in the body. */
export interface SavePolicyInput {
  name: string;
  document: PolicyDocument;
}
