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
/** The identity providers of the console login (GET /auth/providers). */
export type AuthProviders = Schemas['AuthProviders'];

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

/* Pool management (GET /pools, /pools/{pool}/decommission, /rebalance). */

/** The pools, their decommission and the rebalance. */
export type Pools = Schemas['Pools'];
export type PoolStatus = Schemas['PoolStatus'];
/** Progress of a decommission or of the rebalance. */
export type PoolJob = Schemas['PoolJob'];

/* Bucket replication (/buckets/{bucket}/replication, S-BKT-12). */

/** One rule: the keys it selects, the target it sends them to and what it replicates. */
export type ReplicationRule = Schemas['ReplicationRule'];
/** The rules of a bucket (none: not replicated). */
export type ReplicationConfiguration = Schemas['ReplicationConfiguration'];
/** A remote bucket the bucket replicates to; secretKey is sent, never returned. */
export type ReplicationTarget = Schemas['ReplicationTarget'];
/** The replication to one target as the node that answered sees it. */
export type ReplicationTargetStatus = Schemas['ReplicationTargetStatus'];
/** The progress of a resync of the bucket to a target. */
export type ReplicationResync = Schemas['ReplicationResync'];
export type ReplicationStatus = Schemas['ReplicationStatus'];

/* Site replication (/site-replication, S-STO-11). */

export type SiteReplicationSite = Schemas['SiteReplicationSite'];
export type SiteReplicationPeerStatus = Schemas['SiteReplicationPeerStatus'];
/** The group of sites of this deployment and the synchronization with each one. */
export type SiteReplicationInfo = Schemas['SiteReplicationInfo'];

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

/** The video and image rules of a bucket, applied by the media worker. */
export type MediaConfiguration = Schemas['MediaConfiguration'];
export type MediaRule = Schemas['MediaRule'];
/** A rendition preset of a rule (2160p ... 240p). */
export type MediaRendition = NonNullable<MediaRule['renditions']>[number];
export type MediaThumbnails = Schemas['MediaThumbnails'];
/** The variants of an image rule (formats and widths). */
export type MediaImages = Schemas['MediaImages'];
/** A format of the image variants (avif, webp, jpeg, png). */
export type MediaImageFormat = NonNullable<MediaImages['formats']>[number];
/** A variant of an image produced by the media worker. */
export type MediaImageOutput = Schemas['MediaImageOutput'];
/** The version of a video to process again (default the latest). */
export type MediaReprocessRequest = Schemas['MediaReprocessRequest'];
export type MediaSubtitles = Schemas['MediaSubtitles'];
export type SubtitleTrack = Schemas['SubtitleTrack'];
export type SubtitleCue = Schemas['SubtitleCue'];
export type SubtitleCues = Schemas['SubtitleCues'];
export type Subtitles = Schemas['Subtitles'];
/** The formats a subtitle track downloads as. */
export type SubtitleFormat = 'vtt' | 'srt';
/**
 * The processing state of a video or an image by the media worker; alpha
 * reports an image with transparency (its JPEG variants are flattened).
 */
export type MediaStatus = Schemas['MediaStatus'];
export type MediaState = MediaStatus['state'];

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

/* ------------------------------------------------------------------------ */
/* M7b: access rules, presets, block public access (docs/10 §10.1).        */
/* ------------------------------------------------------------------------ */

/** A pattern of keys or paths: prefix, glob or RE2 regex. */
export type KeyPattern = Schemas['KeyPattern'];
export type KeyPatternType = KeyPattern['type'];
/** Conditions of an access rule; all must hold. */
export type AccessRuleConditions = Schemas['AccessRuleConditions'];
/** An access rule of a bucket (not the anonymous access presets of AccessRule). */
export type BucketAccessRule = Schemas['BucketAccessRule'];
/** The access rules of a bucket with their version (etag) and public state. */
export type AccessRules = Schemas['AccessRules'];
/** POST /buckets/{bucket}/access-rules/test. */
export type TestAccessRulesInput = Schemas['TestAccessRulesRequest'];
export type AccessRulesTestResult = Schemas['AccessRulesTestResult'];
/** The outcome of the access rules of the bucket for one key. */
export type AccessRulesKeyResult = Schemas['AccessRulesKeyResult'];
/** A rule as the tester evaluated it (matched, or the first mismatch). */
export type EvaluatedAccessRule = Schemas['EvaluatedAccessRule'];
export type AccessRulesPreset = Schemas['AccessRulesPreset'];
export type AccessRulesPublicStatus = Schemas['AccessRulesPublicStatus'];
export type AccessPresetInput = Schemas['AccessPresetRequest'];
export type BlockPublicMode = Schemas['BlockPublicMode'];
/** The block public access of a bucket or of the cluster (no bucket). */
export type BlockPublic = Schemas['BlockPublic'];
export type BlockPublicInput = Schemas['BlockPublicRequest'];
/** A located problem of an access rule (problem field accessRuleErrors). */
export type AccessRuleError = Schemas['AccessRuleError'];

/* ------------------------------------------------------------------------ */
/* M7b: distributions, key groups, certificates (docs/10 §10.2).           */
/* ------------------------------------------------------------------------ */

/** A distribution: a bucket (or a prefix) served on custom domains. */
export type Distribution = Schemas['Distribution'];
export type DistributionMode = Schemas['DistributionMode'];
export type DistributionAccess = Schemas['DistributionAccess'];
/** How the requests of a path are served; unset fields inherit from the default behavior. */
export type DistributionBehavior = Schemas['DistributionBehavior'];
export type DistributionCors = Schemas['DistributionCORS'];
export type DistributionErrorPage = NonNullable<Distribution['errorPages']>[number];
export type DistributionRedirect = NonNullable<Distribution['redirects']>[number];
export type DistributionRestrictions = NonNullable<Distribution['restrictions']>;
/** What serves a host (GET /routing/resolve). */
export type Route = Schemas['Route'];
export type RouteKind = Route['kind'];
/** POST /distributions/{id}/test. */
export type DistributionTestInput = Schemas['DistributionTestRequest'];
export type DistributionTestResult = Schemas['DistributionTestResult'];
export type RotateOriginSecretInput = Schemas['RotateOriginSecretRequest'];
/** The new origin secret, in clear this time only. */
export type RotateOriginSecretResult = Schemas['RotateOriginSecretResult'];
/** POST /distributions/{id}/sign: a signed URL or signed cookies. */
export type SignDistributionInput = Schemas['SignDistributionRequest'];
export type SignDistributionResult = Schemas['SignDistributionResult'];
export type SignedCookie = Schemas['SignedCookie'];

/** A key group of the signed behaviors, with its public keys. */
export type KeyGroup = Schemas['KeyGroup'];
export type PublicKey = Schemas['PublicKey'];
export type CreateKeyGroupInput = Schemas['CreateKeyGroupRequest'];
/** The key group created, with the private key of its generated key (shown once). */
export type CreateKeyGroupResult = Schemas['CreateKeyGroupResult'];
export type GenerateKeyInput = Schemas['GenerateKeyRequest'];
export type AddKeyInput = Schemas['AddKeyRequest'];
/** The added key; privateKeyPem when the server generated it (shown once). */
export type AddKeyResult = Schemas['AddKeyResult'];

/** An uploaded or ACME certificate (never its key) and the distributions using it. */
export type Certificate = Schemas['Certificate'];
export type CertificateStatus = Schemas['CertificateStatus'];
export type CertificateDetails = Schemas['CertificateDetails'];
/** A certificate of a distribution with tls.certificate auto, issued or pending. */
export type ManagedCertificate = Schemas['ManagedCertificate'];
/** GET /certificates: the certificates and the ACME state. */
export type CertificateList = Schemas['CertificateList'];
export type UploadCertificateInput = Schemas['UploadCertificateRequest'];
/** A DNS provider of the ACME DNS-01 challenge (never its credentials). */
export type DnsProvider = Schemas['DnsProvider'];
export type DnsProviderType = Schemas['DnsProviderType'];
/** Creates or replaces a DNS provider; the empty credentials keep the stored ones. */
export type DnsProviderInput = Schemas['DnsProviderRequest'];
/** The outcome of the test of a DNS provider (a failure is a result). */
export type DnsProviderTestResult = Schemas['DnsProviderTestResult'];
/** The ACME challenges of a distribution (tls.challenge). */
export type AcmeChallenge = NonNullable<NonNullable<Distribution['tls']>['challenge']>;

/**
 * Traffic of the aliases of the distributions (D-DST-08): the per-domain
 * counters of the cache statistics (GET /cache/stats, docs/11 §11.8),
 * summed over the nodes. A console-side view: the statistics are a
 * document of the cache API.
 */
export interface DistributionTraffic {
  hosts: HostTraffic[];
}

/** Counters of one alias of a distribution. */
export interface HostTraffic {
  distribution: string;
  host: string;
  /** Requests answered, every cache result included. */
  requests: number;
  /** Bytes served, from the cache and from the origin. */
  bytes: number;
  /** Share of the cacheable requests served from the cache (0..1). */
  hitRatio: number;
}

/* ------------------------------------------------------------------------ */
/* M7c: cache of the distributions (docs/11).                              */
/* ------------------------------------------------------------------------ */

/** A duration: Go syntax ("90s", "1h30m") or days ("365d"); a number is seconds. */
export type CacheDuration = Schemas['CacheDuration'];
/** A size: bytes, or a number with a unit ("20GiB", "5MB"). */
export type CacheSize = Schemas['CacheSize'];
/** Where a setting of the effective policy comes from (docs/11 §11.1). */
export type CacheLevel = Schemas['CacheLevel'];
/** local, shared or shared+local (docs/11 §11.4). */
export type CacheMode = Schemas['CacheMode'];
/** The query parameters of the cache key ("none", "allowlist:v,w" or the object form). */
export type CacheQueryString = Schemas['CacheQueryString'];
/** The settings of a cache policy; unset fields inherit (levels) or take the standard values. */
export type CachePolicySpec = Schemas['CachePolicySpec'];
/** A reusable cache policy, referenced by name (built-in ones are read-only). */
export type CachePolicy = Schemas['CachePolicy'];
/** POST /cache/preview: the policy completed, its Cache-Control and a summary. */
export type CachePolicyPreview = Schemas['CachePolicyPreview'];
/** The cache settings of a level (cluster, distribution, domain, behavior). */
export type CacheSettings = Schemas['CacheSettings'];
/** The cache settings of the cluster (GET/PUT /cache/settings). */
export type CacheGlobalSettings = Schemas['CacheGlobalSettings'];
/** A CDN or proxy purged on the invalidations of a distribution (secrets masked). */
export type CacheWebhook = Schemas['CacheWebhook'];
export type CacheWebhookType = CacheWebhook['type'];
/** The outcome of a test call of a purge webhook. */
export type CacheWebhookTestResult = Schemas['CacheWebhookTestResult'];
/** The cache settings of a distribution, its domains and its paths. */
export type DistributionCache = Schemas['DistributionCache'];
/** The override of a domain (alias, wildcard alias or a host it matches). */
export type CacheDomainSettings = NonNullable<DistributionCache['domains']>[number];
/** The override of the paths matching a pattern (first match wins). */
export type CacheBehaviorSettings = NonNullable<DistributionCache['behaviors']>[number];
/** The policy applying to a path of a domain, with the level of every setting. */
export type CacheEffectivePolicy = Schemas['CacheEffectivePolicy'];
/** POST /distributions/{id}/invalidations (or /cache/invalidations with all). */
export type CacheInvalidationRequest = Schemas['CacheInvalidationRequest'];
/** The state of an invalidation or of a warm-up on one node. */
export type CacheNodeStatus = Schemas['CacheNodeStatus'];
/** An invalidation with its state on every node. */
export type CacheInvalidation = Schemas['CacheInvalidation'];
/** POST /distributions/{id}/cache/warm. */
export type CacheWarmRequest = Schemas['CacheWarmRequest'];
/** A warm-up with its progress on every node. */
export type CacheWarmJob = Schemas['CacheWarmJob'];
export type CacheWarmNode = CacheWarmJob['nodes'][number];
/** GET /cache/inspect: the key, the effective policy and the entry on every node. */
export type CacheInspection = Schemas['CacheInspection'];
export type CacheInspectionNode = CacheInspection['nodes'][number];
/** Answers by X-Cache value, bytes from the cache and from the origin, hit ratio. */
export type CacheCounters = Schemas['CacheCounters'];
/** The statistics of one domain. */
export type CacheHostStats = Schemas['CacheHostStats'];
/** A requested object (top requests, top misses). */
export type CacheObjectStat = Schemas['CacheObjectStat'];
/** The statistics of one node. */
export type CacheNodeStats = Schemas['CacheNodeStats'];
/** GET /cache/stats: the cluster (summed) and every node. */
export type CacheStats = Schemas['CacheStats'];

/** GET /cache/inspect: an absolute URL, or a path of a distribution. */
export interface CacheInspectInput {
  url: string;
  /** The distribution the URL must be served by (completes a path). */
  distribution?: string;
  /** Request headers of the key ("Accept-Encoding: br"). */
  headers?: string[];
}

/** GET /cache/stats filters. */
export interface CacheStatsQuery {
  distribution?: string;
  host?: string;
  /** Size of the lists of top objects (default 10, at most 100). */
  top?: number;
}

/** GET /distributions/{id}/cache/effective: a domain and a path. */
export interface CacheEffectiveQuery {
  host?: string;
  path?: string;
}

/* Monitoring (D-MON-01..03, docs/07 §7.4): the series of the charts, the
   live trace and logs (Server-Sent Events) and the audit log. */

/** A series of the console charts (GET /metrics): its unit and how the nodes combine. */
export type MetricInfo = Schemas['MetricInfo'];
/** A point of a series: the unix time in seconds and the value, null without sample. */
export type MetricPoint = Schemas['MetricPoint'];
/** The points of one node (error tells why they could not be read). */
export type MetricNodeSeries = Schemas['MetricNodeSeries'];
/** GET /metrics/query: the points of every node and of the cluster. */
export type MetricSeries = Schemas['MetricSeries'];
/** A request of the live trace (SSE event "trace"); the query string is redacted. */
export type TraceEvent = Schemas['TraceEvent'];
/** s3, admin, web or rpc. */
export type TraceType = TraceEvent['type'];
/** An entry of the live logs (SSE event "log"), its secrets redacted. */
export type LogEntry = Schemas['LogEntry'];
/** DEBUG, INFO, WARN or ERROR. */
export type LogLevel = LogEntry['level'];
/** A record of the audit log (GET /audit). */
export type AuditEntry = Schemas['AuditEntry'];
/** The TLS of the listeners of the node that answers GET /info. */
export type TlsInfo = Schemas['TlsInfo'];
/** A certificate of the listeners (never its key). */
export type TlsCertificate = Schemas['TlsCertificate'];
/** The distributed tracing (OpenTelemetry) of the node that answers GET /info. */
export type TelemetryInfo = Schemas['TelemetryInfo'];

/** GET /metrics/query. Durations are Go durations ("1h", "30s"). */
export interface MetricQuery {
  metric: string;
  /** Span of the points (default 1h, at most 24h). */
  range?: string;
  /** Distance between two points (a multiple of 10s; default: at most 360 points). */
  step?: string;
  /** End of the range (RFC 3339; default now). */
  end?: string;
  /** The nodes (default every node). */
  nodes?: string[];
}

/** GET /trace filters; empty fields select everything (every type but rpc). */
export interface TraceFilter {
  nodes?: string[];
  types?: TraceType[];
  /** Operation names (PutObject, "GET /buckets"). */
  apis?: string[];
  buckets?: string[];
  /** Status codes ("404") or classes ("5xx"). */
  statuses?: string[];
  /** Only the failed requests (status >= 400). */
  errorsOnly?: boolean;
  /** Only the requests slower than this Go duration ("500ms"). */
  threshold?: string;
}

/** GET /logs filters. */
export interface LogFilter {
  nodes?: string[];
  /** Minimum level. */
  level?: 'debug' | 'info' | 'warn' | 'error';
  /** Recent entries of each node replayed first (at most 10000). */
  last?: number;
}

/** GET /audit: the recent records kept in memory by every node, newest first. */
export interface AuditQuery {
  nodes?: string[];
  /** The operation (exact, case-insensitive). */
  api?: string;
  bucket?: string;
  /** An access key or a parent user. */
  user?: string;
  type?: 's3' | 'admin';
  /** Status codes ("404") or classes ("5xx"). */
  statuses?: string[];
  errorsOnly?: boolean;
  /** RFC 3339 bounds of the time of the records. */
  since?: string;
  until?: string;
  requestId?: string;
  /** At most this many records (default 100, at most 1000). */
  limit?: number;
}
