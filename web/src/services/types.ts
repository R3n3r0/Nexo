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

/** T with the keys K optional. */
type WithOptional<T, K extends keyof T> = Omit<T, K> & Partial<Pick<T, K>>;

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
/** Progress of the healing of a replaced or wiped drive. */
export type HealingProgress = Schemas['HealingProgress'];
/** Clock offset of a node measured by the node answering GET /info. */
export type ClockSkew = Schemas['ClockSkew'];
/** Drives of an erasure set and the quorum they leave. */
export type ErasureSetInfo = Schemas['ErasureSetInfo'];
/** ok, degraded (write quorum kept), readonly (read quorum only), unavailable. */
export type ErasureSetState = Schemas['ErasureSetState'];

/** Aggregated server/cluster information (GET /info). */
export type ServerInfo = Schemas['ServerInfo'];

/**
 * A bucket. The server always sends `deleteMarkers` and `public`; they are
 * optional on the console side until every mock provides them.
 */
export type Bucket = WithOptional<Schemas['Bucket'], 'deleteMarkers' | 'public'>;
export type CreateBucketInput = Schemas['CreateBucketRequest'];

export type User = Schemas['User'];
export type CreateUserInput = Schemas['CreateUserRequest'];
export type UpdateUserInput = Schemas['UpdateUserRequest'];

export type Group = Schemas['Group'];
export type CreateGroupInput = Schemas['CreateGroupRequest'];
export type UpdateGroupInput = Schemas['UpdateGroupRequest'];

/** An IAM policy with its JSON document (AWS format). */
export type Policy = Schemas['Policy'];
export type PolicyDocument = Schemas['PolicyDocument'];
export type PolicyStatement = Schemas['PolicyStatement'];
/** A policy document, or its JSON text (server errors are then located in that text). */
export type PolicyDocumentOrText = Schemas['PolicyDocumentOrText'];
/** A located error of a malformed policy (problem code malformed_policy). */
export type PolicyError = Schemas['PolicyError'];
export type CreatePolicyInput = Schemas['CreatePolicyRequest'];
/** POST /policies/attach and /policies/detach: exactly one of user and group. */
export type AttachPolicyInput = Schemas['AttachRequest'];
export type PolicyEntities = Schemas['PolicyEntities'];
export type ValidatePolicyInput = Schemas['ValidatePolicyRequest'];
export type ValidationReport = Schemas['ValidationReport'];
export type SimulateInput = Schemas['SimulateRequest'];
export type Simulation = Schemas['Simulation'];
export type EvaluatedStatement = Schemas['EvaluatedStatement'];
/** Where an evaluated statement comes from (root, iam, group, inline, bucket). */
export type PolicySource = EvaluatedStatement['source'];

/** Effective permissions of a principal (GET /me/permissions). */
export type Permissions = Schemas['Permissions'];

/* Share links, presigned URLs and POST policies. */

export type SharePermission = Schemas['SharePermission'];
/** A managed, revocable share link. */
export type Share = Schemas['Share'];
export type CreateShareInput = Schemas['CreateShareRequest'];
/** The download link of one object of a prefix share (revoked with the share). */
export type ShareLink = Schemas['ShareLink'];
/** A page of the per-object links of a prefix download share. */
export type ShareLinkList = Schemas['ShareLinkList'];
export type RevokeAllResult = Schemas['RevokeAllResult'];
export type PresignInput = Schemas['PresignRequest'];
/** A raw presigned URL: not revocable, valid until it expires. */
export type PresignedUrl = Schemas['PresignedURL'];
export type PostPolicyInput = Schemas['PostPolicyRequest'];
export type PostPolicy = Schemas['PostPolicy'];
export type PostCondition = Schemas['PostCondition'];

/* Dynamic configuration (GET/PUT /config...). */

export type ConfigSettings = Schemas['ConfigSettings'];
export type ConfigSubsystem = Schemas['ConfigSubsystem'];
export type ConfigDocument = Schemas['ConfigDocument'];
export type ConfigKey = Schemas['ConfigKey'];
export type ConfigKeyType = ConfigKey['type'];
export type ConfigSubsystemSchema = Schemas['ConfigSubsystemSchema'];

export type AccessKey = Schemas['AccessKey'];
/** The access key just created: the only time the secret is visible. */
export type CreatedAccessKey = AccessKey & { secretKey: string };
export type CreateAccessKeyInput = Schemas['CreateAccessKeyRequest'];
export type UpdateAccessKeyInput = Schemas['UpdateAccessKeyRequest'];

/* Healing jobs (POST/GET /heal, GET /heal/{id}). */

/** What to heal: no bucket = format and every bucket; recursive = the objects too. */
export type HealRequest = Schemas['HealRequest'];
export type HealJob = Schemas['HealJob'];
export type HealJobState = Schemas['HealJobState'];
/** Per-drive counters of a healing job. */
export type HealJobDrive = Schemas['HealJobDrive'];
/** An item that needed healing, was dangling or failed. */
export type HealItem = Schemas['HealItem'];
export type HealItemDrive = Schemas['HealItemDrive'];
/** State of an item on a drive before and after the healing. */
export type HealDriveState = Schemas['HealDriveState'];

/* Pages of the list endpoints ({items, nextCursor}). */

export type BucketList = Schemas['BucketList'];
export type UserList = Schemas['UserList'];
export type GroupList = Schemas['GroupList'];
export type PolicyList = Schemas['PolicyList'];
export type AccessKeyList = Schemas['AccessKeyList'];
export type ShareList = Schemas['ShareList'];
export type HealJobList = Schemas['HealJobList'];

/* Console-side inputs composed from the wire types. */

export interface RemoveBucketOptions {
  /** Delete the bucket even when it still contains objects. */
  force?: boolean;
}

/**
 * PUT /policies/{name}: the name travels in the path, the document in the
 * body. Send the JSON text to have malformed_policy errors located in it.
 */
export interface SavePolicyInput {
  name: string;
  document: PolicyDocumentOrText;
}

/** GET /access-keys filter: the caller's keys by default. */
export interface AccessKeyFilter {
  /** Keys of this user (admin:ListServiceAccounts for another user). */
  user?: string;
}

/** GET /shares filter: the caller's links by default. */
export interface ShareFilter {
  /** Links of this owner (admin:ManageShares for another owner). */
  owner?: string;
  bucket?: string;
}

/** GET /me/permissions query. */
export interface PermissionsQuery {
  /** Another user (admin:GetUser); the caller by default. */
  user?: string;
  /** Evaluate bucket actions on this bucket and object actions on prefix*. */
  bucket?: string;
  prefix?: string;
}

/* ------------------------------------------------------------------------ */
/* Storage: usage, bucket settings, anonymous access, objects and uploads.  */
/* ------------------------------------------------------------------------ */

/** Data usage, total and per bucket (GET /usage). */
export type DataUsage = Schemas['DataUsage'];
export type BucketUsage = Schemas['BucketUsage'];

export type BucketVersioning = Schemas['BucketVersioning'];
export type BucketQuota = Schemas['BucketQuota'];
/** Tag set of a bucket or an object (key -> value). */
export type TagSet = Schemas['Tags']['tags'];
export type BucketObjectLock = Schemas['BucketObjectLock'];
export type BucketEncryption = Schemas['BucketEncryption'];

/** Anonymous access preset of a prefix ("custom" when the policy is hand written). */
export type AccessPreset = Schemas['AccessPreset'];
export type AccessRule = Schemas['AccessRule'];
export type BucketAccess = Schemas['BucketAccess'];
export type SetAccessInput = Schemas['SetAccessRequest'];

/** An object (version) of the object browser. */
export type ObjectEntry = Schemas['ObjectEntry'];
/** A page of the folder view of a bucket. */
export type ObjectList = Schemas['ObjectList'];
/** Metadata, tags and first versions of an object (GET objects/stat). */
export type ObjectDetails = Schemas['ObjectDetails'];
export type VersionList = Schemas['VersionList'];
export type ObjectRef = Schemas['ObjectRef'];
export type DeleteObjectsInput = Schemas['DeleteObjectsRequest'];
export type DeleteObjectsResult = Schemas['DeleteObjectsResult'];
export type RenameInput = Schemas['RenameRequest'];
export type RenameResult = Schemas['RenameResult'];
export type ZipInput = Schemas['ZipRequest'];

/** A multipart upload proxied by the console, with the limits of its parts. */
export type Upload = Schemas['Upload'];
export type PendingUpload = Schemas['PendingUpload'];
export type UploadList = Schemas['UploadList'];
export type Part = Schemas['Part'];
export type PartList = Schemas['PartList'];
export type CompletedPart = Schemas['CompleteUploadRequest']['parts'][number];
export type CreateUploadInput = Schemas['CreateUploadRequest'];

/** Query of the folder view (GET /buckets/{bucket}/objects). */
export interface ListObjectsQuery {
  prefix?: string;
  /** "/" by default; "" lists recursively. */
  delimiter?: string;
  cursor?: string;
  limit?: number;
}

/** Cursor pagination of the list endpoints. */
export interface PageQuery {
  cursor?: string;
  limit?: number;
}

/** Cancellation and progress of a transfer. */
export interface TransferOptions {
  signal?: AbortSignal;
  /** Bytes sent so far out of total. */
  onProgress?: (loaded: number, total: number) => void;
}

/** POST /buckets/{bucket}/objects/upload: a small object in one request. */
export interface PutObjectInput {
  bucket: string;
  key: string;
  body: Blob;
  /** Defaults to the type of the blob, then application/octet-stream. */
  contentType?: string;
  /** Server-side encryption of the object (the bucket default without). */
  encryption?: UploadEncryption;
}

/** PUT /buckets/{bucket}/uploads/{uploadId}/parts/{n}. */
export interface UploadPartInput {
  bucket: string;
  key: string;
  uploadId: string;
  partNumber: number;
  body: Blob;
}

/** The head of a text object, for the preview. */
export interface TextPreview {
  text: string;
  /** The object is larger than the bytes read. */
  truncated: boolean;
  /** Size of the whole object, when known. */
  size?: number;
  contentType: string;
}

/* ------------------------------------------------------------------------ */
/* M7: lifecycle, event notifications, CORS, object lock, checksums, KMS.  */
/* ------------------------------------------------------------------------ */

/** The lifecycle rules of a bucket (S3 LifecycleConfiguration, D-BKT-03). */
export type LifecycleConfiguration = Schemas['LifecycleConfiguration'];
export type LifecycleRule = Schemas['LifecycleRule'];
/** The objects of a rule: every criterion set must match (none: the whole bucket). */
export type LifecycleFilter = Schemas['LifecycleFilter'];
/** Exactly one of days, date and expiredObjectDeleteMarker. */
export type LifecycleExpiration = Schemas['LifecycleExpiration'];
export type LifecycleNoncurrentExpiration = Schemas['LifecycleNoncurrentExpiration'];
export type LifecycleAbortUpload = Schemas['LifecycleAbortUpload'];
export type LifecycleTransition = Schemas['LifecycleTransition'];
export type LifecycleNoncurrentTransition = Schemas['LifecycleNoncurrentTransition'];
/** What POST /buckets/{bucket}/lifecycle/apply did. */
export type LifecycleApplyResult = Schemas['LifecycleApplyResult'];

/** The event subscriptions of a bucket (S3 NotificationConfiguration, D-BKT-04). */
export type BucketNotifications = Schemas['BucketNotifications'];
export type EventSubscription = Schemas['EventSubscription'];
/** A notification target (notify_* subsystem) with its delivery state. */
export type NotifyTarget = Schemas['NotifyTarget'];
export type NotifyTargetType = NotifyTarget['type'];
/** The outcome of a test event sent to a target. */
export type NotifyTargetTest = Schemas['NotifyTargetTest'];

/** The CORS rules of a bucket, evaluated in order. */
export type CorsConfiguration = Schemas['CorsConfiguration'];
export type CorsRule = Schemas['CorsRule'];

/** Object Lock retention mode. */
export type RetentionMode = NonNullable<Schemas['ObjectRetention']['mode']>;
/** The retention of a version: no mode when it has none. */
export type ObjectRetention = Schemas['ObjectRetention'];
/**
 * PUT objects/retention: a mode with a future date, or neither to remove
 * it; bypassGovernance shortens or removes a GOVERNANCE retention.
 */
export type SetObjectRetentionInput = Schemas['SetObjectRetentionRequest'];
export type ObjectLegalHold = Schemas['ObjectLegalHold'];
export type LegalHoldStatus = 'ON' | 'OFF';

/** An S3 additional checksum (base64 value). */
export type ObjectChecksum = Schemas['Checksum'];
export type ObjectPart = Schemas['ObjectPart'];
/** Checksum and parts of a version (GET objects/attributes). */
export type ObjectAttributes = Schemas['ObjectAttributes'];
/** Server-side encryption of a version. */
export type ObjectEncryption = Schemas['ObjectEncryption'];

/** State of the KMS of the server-side encryption (GET /kms/status). */
export type KmsStatus = Schemas['KmsStatus'];
/** The KMS as GET /info reports it, with the names of its first keys. */
export type KmsInfo = Schemas['KmsInfo'];
export type KmsKey = Schemas['KmsKey'];

/** Query of GET objects/attributes. */
export interface AttributesQuery {
  versionId?: string;
  /** Parts per page, 1 to 1000. */
  maxParts?: number;
  /** The parts after this number. */
  partNumberMarker?: number;
}

/**
 * Server-side encryption asked for by an upload (the S3 headers
 * x-amz-server-side-encryption[-aws-kms-key-id]); none lets the default
 * encryption of the bucket apply.
 */
export interface UploadEncryption {
  /** AES256 (SSE-S3) or aws:kms (SSE-KMS). */
  algorithm: 'AES256' | 'aws:kms';
  /** The key of SSE-KMS; the default key of the KMS when absent. */
  kmsKeyId?: string;
}

/** POST /buckets/{bucket}/uploads: the body plus the encryption of the object. */
export interface CreateUploadOptions extends CreateUploadInput {
  encryption?: UploadEncryption;
}
