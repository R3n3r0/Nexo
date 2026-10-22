/**
 * Service contracts of the console (docs/12 §12.5).
 *
 * Every interface has two implementations: `http/` talks to the Admin API,
 * `mock/` keeps state in memory for tests and the demo mode. Components only
 * depend on these interfaces, obtained through `useServices()`.
 *
 * Every method rejects with an `ApiError` (see ./errors.ts) on failure.
 */
import type {
  AccessKey,
  AccessPresetInput,
  AuthProviders,
  CacheEffectivePolicy,
  CacheEffectiveQuery,
  CacheGlobalSettings,
  CacheInspection,
  CacheInspectInput,
  CacheInvalidation,
  CacheInvalidationRequest,
  CachePolicy,
  CachePolicyPreview,
  CacheStats,
  CacheStatsQuery,
  CacheWarmJob,
  CacheWarmRequest,
  CacheWebhook,
  CacheWebhookTestResult,
  DistributionCache,
  AccessRules,
  AccessRulesPublicStatus,
  AccessRulesTestResult,
  AddKeyInput,
  AddKeyResult,
  BlockPublic,
  BlockPublicInput,
  BucketAccessRule,
  Certificate,
  CertificateList,
  CreateKeyGroupInput,
  CreateKeyGroupResult,
  Distribution,
  DistributionTestInput,
  DistributionTestResult,
  DistributionTraffic,
  DnsProvider,
  DnsProviderInput,
  DnsProviderTestResult,
  KeyGroup,
  PublicKey,
  RotateOriginSecretInput,
  RotateOriginSecretResult,
  Route,
  SignDistributionInput,
  SignDistributionResult,
  TestAccessRulesInput,
  UploadCertificateInput,
  AccessKeyFilter,
  AttachPolicyInput,
  AuditEntry,
  AuditQuery,
  LogEntry,
  LogFilter,
  MetricInfo,
  MetricQuery,
  MetricSeries,
  TraceEvent,
  TraceFilter,
  AttributesQuery,
  Bucket,
  BucketAccess,
  BucketEncryption,
  BucketNotifications,
  BucketObjectLock,
  BucketQuota,
  CompletedPart,
  ConfigDocument,
  ConfigSettings,
  ConfigSubsystem,
  ConfigSubsystemSchema,
  CreateAccessKeyInput,
  CreateBucketInput,
  CreatedAccessKey,
  CreateGroupInput,
  CreatePolicyInput,
  CreateShareInput,
  CorsConfiguration,
  CreateUploadOptions,
  CreateUserInput,
  DataUsage,
  DeleteObjectsInput,
  DeleteObjectsResult,
  Group,
  HealJob,
  HealRequest,
  Pools,
  ReplicationConfiguration,
  ReplicationResync,
  ReplicationStatus,
  ReplicationTarget,
  SiteReplicationInfo,
  SiteReplicationSite,
  KmsKey,
  KmsStatus,
  LegalHoldStatus,
  LifecycleApplyResult,
  LifecycleConfiguration,
  ListObjectsQuery,
  MediaConfiguration,
  MediaReprocessRequest,
  SubtitleCue,
  SubtitleCues,
  SubtitleFormat,
  SubtitleTrack,
  MediaStatus,
  NotifyTarget,
  NotifyTargetTest,
  ObjectAttributes,
  ObjectDetails,
  ObjectEntry,
  ObjectList,
  ObjectRetention,
  PageQuery,
  Part,
  Permissions,
  PermissionsQuery,
  Policy,
  PolicyEntities,
  PostPolicy,
  PostPolicyInput,
  PresignedUrl,
  PresignInput,
  PutObjectInput,
  RemoveBucketOptions,
  RenameInput,
  RenameResult,
  SavePolicyInput,
  ServerInfo,
  Session,
  SetAccessInput,
  SetObjectRetentionInput,
  Share,
  ShareFilter,
  ShareLinkList,
  SimulateInput,
  Simulation,
  TagSet,
  TextPreview,
  TransferOptions,
  UpdateAccessKeyInput,
  UpdateGroupInput,
  UpdateUserInput,
  Upload,
  UploadList,
  UploadPartInput,
  User,
  ValidatePolicyInput,
  ValidationReport,
  VersioningState,
  VersionList,
  ZipInput,
} from './types';
import type { ApiError } from './errors';

/** Console session (cookie + CSRF token managed by the implementation). */
export interface SessionService {
  /** Authenticates with an access/secret key pair and opens a session. */
  login(accessKey: string, secretKey: string): Promise<Session>;
  /** Closes the current session; never fails when no session exists. */
  logout(): Promise<void>;
  /** Returns the current session or rejects with a 401 ApiError. */
  current(): Promise<Session>;
  /** The identity providers the login page offers (public). */
  providers(): Promise<AuthProviders>;
  /** Authenticates a user of the LDAP directory and opens a session. */
  loginLDAP(username: string, password: string): Promise<Session>;
  /**
   * URL the browser navigates to for the OpenID Connect login: the server
   * redirects to the provider, which comes back with a session cookie.
   */
  openIDLoginURL(): string;
}

/** Server and cluster information. */
export interface InfoService {
  serverInfo(): Promise<ServerInfo>;
  /** Data usage, total and per bucket (GET /usage). */
  usage(): Promise<DataUsage>;
}

/**
 * Healing jobs (D-CLU-02): POST /heal starts a background job, GET /heal
 * and GET /heal/{id} report its progress (any node answers). A
 * single-drive deployment has nothing to heal: every call rejects with a
 * 501 not_implemented ApiError.
 */
export interface HealService {
  /**
   * Starts a job: without bucket the format of the drives and every
   * bucket, with recursive the objects too. An identical running job is
   * returned instead of a new one; too many running jobs reject with 429.
   */
  start(request: HealRequest): Promise<HealJob>;
  /** Running and recently finished jobs, newest first. */
  list(): Promise<HealJob[]>;
  get(id: string): Promise<HealJob>;
}

/**
 * Pool management (D-CLU-03): GET /pools reports the pools, their usage,
 * their decommission and the rebalance; a decommission drains a pool so
 * that it can be removed, a rebalance evens out the usage of the pools.
 * Pools are 1-based positions in the command line. A single-drive
 * deployment has no pools to manage: every call rejects with a 501
 * not_implemented ApiError.
 */
export interface PoolsService {
  list(): Promise<Pools>;
  /** Starts the decommission of a pool (400 for the first pool). */
  decommission(pool: number): Promise<void>;
  /** Cancels it: the pool takes new objects again. */
  cancelDecommission(pool: number): Promise<void>;
  startRebalance(): Promise<void>;
  stopRebalance(): Promise<void>;
}

/** Bucket management. */
export interface BucketService {
  list(): Promise<Bucket[]>;
  get(name: string): Promise<Bucket>;
  create(input: CreateBucketInput): Promise<Bucket>;
  remove(name: string, options?: RemoveBucketOptions): Promise<void>;
  getVersioning(name: string): Promise<VersioningState>;
  setVersioning(name: string, status: Exclude<VersioningState, 'off'>): Promise<VersioningState>;
  getQuota(name: string): Promise<BucketQuota>;
  /** Sets the hard quota in bytes; 0 removes it. */
  setQuota(name: string, sizeBytes: number): Promise<BucketQuota>;
  getTags(name: string): Promise<TagSet>;
  /** Replaces the tags; an empty set removes them. */
  setTags(name: string, tags: TagSet): Promise<TagSet>;
  getObjectLock(name: string): Promise<BucketObjectLock>;
  /**
   * Enables object lock (versioned buckets only; it cannot be disabled)
   * with its default retention: mode and days or years, or none.
   */
  setObjectLock(name: string, config: BucketObjectLock): Promise<BucketObjectLock>;
  /** Default encryption; null when the bucket has none. */
  getEncryption(name: string): Promise<BucketEncryption | null>;
  /** Sets the default encryption, or removes it with null. */
  setEncryption(
    name: string,
    encryption: BucketEncryption | null,
  ): Promise<BucketEncryption | null>;
  /** The bucket policy as JSON text; null when the bucket has none. */
  getPolicy(name: string): Promise<string | null>;
  /**
   * Stores the bucket policy from its JSON text, so that malformed_policy
   * errors (ApiError.errors) are located by line and column in that text.
   */
  setPolicy(name: string, text: string): Promise<void>;
  deletePolicy(name: string): Promise<void>;
  /** Anonymous access presets per prefix. */
  getAccess(name: string): Promise<BucketAccess>;
  setAccess(name: string, input: SetAccessInput): Promise<BucketAccess>;
  /** Removes the anonymous access of a prefix ("" is the whole bucket). */
  removeAccess(name: string, prefix: string): Promise<BucketAccess>;
  /** CORS rules, evaluated in order; no rule when the bucket has none. */
  getCors(name: string): Promise<CorsConfiguration>;
  /**
   * Replaces the CORS rules (no rule removes them). A rejected
   * configuration fails with invalid_configuration and ApiError.ruleErrors.
   */
  setCors(name: string, config: CorsConfiguration): Promise<CorsConfiguration>;
}

/**
 * Lifecycle rules of a bucket (D-BKT-03): the S3 LifecycleConfiguration
 * in structured form, the document the S3 API reads and writes.
 */
export interface LifecycleService {
  /** The rules; none when the bucket has no configuration. */
  get(bucket: string): Promise<LifecycleConfiguration>;
  /**
   * Replaces the rules (none removes the configuration); resolves to the
   * rules as stored (generated ids). Every rule is validated on its own: a
   * rejected configuration fails with invalid_configuration and
   * ApiError.ruleErrors (1-based rule index).
   */
  set(bucket: string, config: LifecycleConfiguration): Promise<LifecycleConfiguration>;
  remove(bucket: string): Promise<void>;
  /** Applies the rules at once (s3:PutLifecycleConfiguration + admin:ApplyLifecycle). */
  apply(bucket: string): Promise<LifecycleApplyResult>;
}

/** Event subscriptions of a bucket and notification targets (D-BKT-04). */
export interface NotificationService {
  /** The subscriptions; none when the bucket has no configuration. */
  get(bucket: string): Promise<BucketNotifications>;
  /** Replaces the subscriptions, validated one by one like the lifecycle rules. */
  set(bucket: string, config: BucketNotifications): Promise<BucketNotifications>;
  remove(bucket: string): Promise<void>;
  /** Configured targets with their delivery state (admin:ConfigUpdate). */
  targets(): Promise<NotifyTarget[]>;
  /**
   * Sends an s3:TestEvent to a target ("<id>:<type>" or its ARN); a
   * failed delivery resolves with delivered false.
   */
  test(target: string): Promise<NotifyTargetTest>;
}

/**
 * Replication of a bucket to remote buckets (S-BKT-12): the targets
 * (/buckets/{bucket}/replication/targets, admin:GetBucketTarget /
 * admin:SetBucketTarget), the rules (s3:Get/PutReplicationConfiguration),
 * the status and the resync (s3:ResetBucketReplicationState).
 */
export interface ReplicationService {
  /** The rules; none when the bucket is not replicated. */
  get(bucket: string): Promise<ReplicationConfiguration>;
  /** Replaces the rules (they name targets of the bucket; versioning must be enabled). */
  set(bucket: string, config: ReplicationConfiguration): Promise<ReplicationConfiguration>;
  remove(bucket: string): Promise<void>;
  /** The targets, without their secret keys. */
  targets(bucket: string): Promise<ReplicationTarget[]>;
  /** Creates a target (no arn) or updates one (an empty secret keeps the stored one). */
  setTarget(bucket: string, target: ReplicationTarget): Promise<ReplicationTarget>;
  /** Removes a target no rule uses (409 otherwise). */
  removeTarget(bucket: string, arn: string): Promise<void>;
  status(bucket: string): Promise<ReplicationStatus>;
  /** Starts a resync of the versions written before the rules of a target. */
  resync(bucket: string, target: string): Promise<ReplicationResync>;
}

/**
 * Site replication (S-STO-11): several deployments sharing the root
 * credentials keep their IAM, policies and buckets in sync
 * (admin:SiteReplicationInfo / Add / Remove).
 */
export interface SiteReplicationService {
  info(): Promise<SiteReplicationInfo>;
  /** Groups the sites (this one among them). */
  add(sites: SiteReplicationSite[]): Promise<SiteReplicationInfo>;
  /** Dissolves the group on every site reached. */
  remove(): Promise<void>;
  /** Asks for a round of synchronization now. */
  sync(): Promise<void>;
}

/**
 * Video rules of a bucket and the outputs of the media worker
 * (/buckets/{bucket}/media, admin:GetBucketMedia / admin:SetBucketMedia).
 */
export interface MediaService {
  /** The rules; none when the bucket has no configuration. */
  get(bucket: string): Promise<MediaConfiguration>;
  /**
   * Replaces the rules (none removes the configuration); resolves to the
   * rules as stored (normalized suffixes and renditions). Every rule is
   * validated on its own: a rejected configuration fails with
   * invalid_configuration and ApiError.ruleErrors (1-based rule index).
   */
  set(bucket: string, config: MediaConfiguration): Promise<MediaConfiguration>;
  remove(bucket: string): Promise<void>;
  /** Queues the processing of a version of a video again (400 when no rule selects it). */
  reprocess(bucket: string, request: MediaReprocessRequest): Promise<void>;
  /** The processing state of a version of a video (state none when never processed). */
  status(bucket: string, key: string, versionId?: string): Promise<MediaStatus>;
  /**
   * The subtitle tracks of a video that apply to its content: the manual
   * ones and the automatic ones the worker derived from this content.
   */
  subtitles(bucket: string, key: string, versionId?: string): Promise<SubtitleTrack[]>;
  /** A track and its cues; 404 when the video has no current track of that language. */
  subtitleCues(
    bucket: string,
    key: string,
    lang: string,
    versionId?: string,
  ): Promise<SubtitleCues>;
  /**
   * Stores cues as the manual track of a language (replacing an automatic
   * one, which the worker then leaves alone); resolves to the track.
   */
  putSubtitleCues(
    bucket: string,
    key: string,
    lang: string,
    cues: SubtitleCue[],
    versionId?: string,
  ): Promise<SubtitleTrack>;
  /** Stores a WebVTT or SubRip document as the manual track of a language. */
  uploadSubtitles(
    bucket: string,
    key: string,
    lang: string,
    document: string,
    versionId?: string,
  ): Promise<SubtitleTrack>;
  deleteSubtitles(bucket: string, key: string, lang: string, versionId?: string): Promise<void>;
  /** URL downloading a track as WebVTT or SubRip (the browser follows it with the session). */
  subtitleDownloadUrl(
    bucket: string,
    key: string,
    lang: string,
    format: SubtitleFormat,
    versionId?: string,
  ): string;
}

/** KMS of the server-side encryption (/kms). */
export interface KmsService {
  /** Rejects with 501 when the server has no KMS service. */
  status(): Promise<KmsStatus>;
  keys(prefix?: string): Promise<KmsKey[]>;
  /** Creates a key usable for SSE-KMS; 409 when it exists. */
  createKey(id: string): Promise<KmsKey>;
}

/** Object browser operations (GET/DELETE /buckets/{bucket}/objects...). */
export interface ObjectService {
  /** One page of the folder view under query.prefix. */
  list(bucket: string, query?: ListObjectsQuery): Promise<ObjectList>;
  stat(bucket: string, key: string, versionId?: string): Promise<ObjectDetails>;
  /** Versions and delete markers of one key, newest first. */
  versions(bucket: string, key: string, query?: PageQuery): Promise<VersionList>;
  /** Creates the empty object "prefix/". */
  createFolder(bucket: string, prefix: string): Promise<ObjectEntry>;
  /** Deletes objects, versions and prefixes (recursive). */
  remove(bucket: string, input: DeleteObjectsInput): Promise<DeleteObjectsResult>;
  /** Moves an object or a prefix (copy then delete). */
  rename(bucket: string, input: RenameInput): Promise<RenameResult>;
  setTags(bucket: string, key: string, tags: TagSet, versionId?: string): Promise<TagSet>;
  /** Object Lock retention of a version: no mode when it has none. */
  getRetention(bucket: string, key: string, versionId?: string): Promise<ObjectRetention>;
  /**
   * Sets or removes the retention of a version. Shortening or removing a
   * GOVERNANCE retention needs input.bypassGovernance
   * (s3:BypassGovernanceRetention); a COMPLIANCE one never shortens.
   */
  setRetention(
    bucket: string,
    key: string,
    input: SetObjectRetentionInput,
    versionId?: string,
  ): Promise<ObjectRetention>;
  /** Legal hold of a version (OFF when never set). */
  getLegalHold(bucket: string, key: string, versionId?: string): Promise<LegalHoldStatus>;
  setLegalHold(
    bucket: string,
    key: string,
    status: LegalHoldStatus,
    versionId?: string,
  ): Promise<LegalHoldStatus>;
  /** Checksum and a page of the parts of a version (S3 GetObjectAttributes). */
  attributes(bucket: string, key: string, query?: AttributesQuery): Promise<ObjectAttributes>;
  /**
   * Makes a previous version the latest again (a copy of the version onto
   * its key, the history is kept); resolves to the new latest version.
   */
  restoreVersion(bucket: string, key: string, versionId: string): Promise<ObjectEntry>;
  /** URL that downloads the object as an attachment (session cookie). */
  downloadUrl(bucket: string, key: string, versionId?: string): string;
  /** URL that renders a safe preview inline (others are served as attachments). */
  previewUrl(bucket: string, key: string, versionId?: string): string;
  /**
   * URL of the HLS master playlist of an MP4 video (session cookie), or
   * undefined when the backend cannot stream videos (the demo mocks).
   */
  streamUrl(bucket: string, key: string, versionId?: string): string | undefined;
  /**
   * URL of the JPEG poster the media worker derived from a video, or
   * undefined when the backend serves none (the demo mocks).
   */
  posterUrl(bucket: string, key: string, versionId?: string): string | undefined;
  /**
   * URL of the WebVTT map of the timeline thumbnails of a video (its cues
   * point to the sprite), or undefined when the backend serves none.
   */
  thumbnailsUrl(bucket: string, key: string, versionId?: string): string | undefined;
  /** Reads at most maxBytes from the start of a text object. */
  previewText(
    bucket: string,
    key: string,
    options: { maxBytes: number; versionId?: string; signal?: AbortSignal },
  ): Promise<TextPreview>;
  /**
   * Downloads a zip archive of keys and prefixes: the browser writes it to
   * disk as the server streams it. Resolves once the download starts;
   * rejects with the problem of a refused request.
   */
  downloadZip(bucket: string, input: ZipInput, signal?: AbortSignal): Promise<void>;
}

/** Uploads proxied by the console: single request or multipart. */
export interface UploadService {
  /** Uploads a small object in one request. */
  putObject(input: PutObjectInput, options?: TransferOptions): Promise<ObjectEntry>;
  /**
   * Starts a multipart upload, with the encryption of the object; the
   * answer carries the part size limits.
   */
  create(bucket: string, key: string, input?: CreateUploadOptions): Promise<Upload>;
  uploadPart(input: UploadPartInput, options?: TransferOptions): Promise<Part>;
  complete(
    bucket: string,
    key: string,
    uploadId: string,
    parts: CompletedPart[],
  ): Promise<ObjectEntry>;
  abort(bucket: string, key: string, uploadId: string): Promise<void>;
  /** Pending multipart uploads of a bucket. */
  list(bucket: string, query?: PageQuery & { prefix?: string }): Promise<UploadList>;
}

/** IAM users (/users). */
export interface UserService {
  /** Every user (all the pages of GET /users). */
  list(): Promise<User[]>;
  get(accessKey: string): Promise<User>;
  create(input: CreateUserInput): Promise<User>;
  /** Partial update: status, secret, policies, group memberships (replaced). */
  update(accessKey: string, input: UpdateUserInput): Promise<User>;
  remove(accessKey: string): Promise<void>;
}

/** IAM groups (/groups). */
export interface GroupService {
  list(): Promise<Group[]>;
  get(name: string): Promise<Group>;
  create(input: CreateGroupInput): Promise<Group>;
  /** Partial update: status, policies, members to add and to remove. */
  update(name: string, input: UpdateGroupInput): Promise<Group>;
  remove(name: string): Promise<void>;
}

/** IAM policies (/policies), their validation and the simulator. */
export interface PolicyService {
  /** Built-in and stored policies. */
  list(): Promise<Policy[]>;
  get(name: string): Promise<Policy>;
  /** Creates a policy; 409 conflict when the name exists. */
  create(input: CreatePolicyInput): Promise<Policy>;
  /** Creates or replaces a policy (PUT). */
  save(input: SavePolicyInput): Promise<Policy>;
  remove(name: string): Promise<void>;
  /** Validates without storing; errors are located in the JSON text when one is sent. */
  validate(input: ValidatePolicyInput): Promise<ValidationReport>;
  /** Evaluates a request with the enforcement engine. */
  simulate(input: SimulateInput): Promise<Simulation>;
  /** Users and groups the policy is attached to. */
  entities(name: string): Promise<PolicyEntities>;
  attach(input: AttachPolicyInput): Promise<void>;
  detach(input: AttachPolicyInput): Promise<void>;
}

/** Service accounts (access keys derived from a user, /access-keys). */
export interface AccessKeyService {
  /** The caller's keys, or those of filter.user (every key for an admin without user). */
  list(filter?: AccessKeyFilter): Promise<AccessKey[]>;
  get(accessKey: string): Promise<AccessKey>;
  /** Creates a key; the result carries the secret, visible only this time. */
  create(input: CreateAccessKeyInput): Promise<CreatedAccessKey>;
  update(accessKey: string, input: UpdateAccessKeyInput): Promise<AccessKey>;
  remove(accessKey: string): Promise<void>;
}

/** Managed share links (/shares), raw presigned URLs and POST policies. */
export interface ShareService {
  /** Active links: the caller's own, or filtered by owner/bucket for an admin. */
  list(filter?: ShareFilter): Promise<Share[]>;
  get(id: string): Promise<Share>;
  /** Creates a revocable link signed by a dedicated service account. */
  create(input: CreateShareInput): Promise<Share>;
  /**
   * One download link per object of a prefix download share, a page at a
   * time: every link is signed by the share and revoked with it.
   */
  links(id: string, query?: PageQuery): Promise<ShareLinkList>;
  /** Revokes a link with immediate effect. */
  revoke(id: string): Promise<void>;
  /** Revokes every link of an owner (the caller by default); resolves to the count. */
  revokeAll(owner?: string): Promise<number>;
  /** Raw presigned URL signed with the caller's credentials: NOT revocable. */
  presign(input: PresignInput): Promise<PresignedUrl>;
  /** Signed POST policy form for browser uploads. */
  postPolicy(input: PostPolicyInput): Promise<PostPolicy>;
}

/** Dynamic server configuration (/config). Sensitive values come back masked. */
export interface ConfigService {
  /** Every known subsystem with its settings. */
  list(): Promise<ConfigSubsystem[]>;
  /** Known subsystems and keys (types, defaults, sensitivity). */
  schema(): Promise<ConfigSubsystemSchema[]>;
  get(subsystem: string): Promise<ConfigSubsystem>;
  /** Merges the settings; sending the mask back keeps a stored secret. */
  set(subsystem: string, settings: ConfigSettings): Promise<ConfigSubsystem>;
  /** Removes the given keys, or the whole subsystem without keys. */
  reset(subsystem: string, keys?: string[]): Promise<void>;
  /** The whole configuration, secrets included. */
  exportAll(): Promise<ConfigDocument>;
  /** Replaces the whole configuration (validated first). */
  importAll(document: ConfigDocument): Promise<void>;
}

/** Effective permissions (GET /me/permissions), for the permission-aware UI. */
export interface PermissionService {
  /** Allowed and denied catalog actions of the caller, or of query.user. */
  get(query?: PermissionsQuery): Promise<Permissions>;
}

/**
 * Access rules of the buckets (D-ACC-01..04, docs/10 §10.1): prefix, glob
 * and RE2 regex rules evaluated by the policy engine, the public presets
 * and the block public access. Every change is atomic: with ifMatch (the
 * etag of the rules read) a concurrent change rejects with 412
 * precondition_failed (ApiError.isPreconditionFailed); an invalid rule
 * rejects with 400 invalid_access_rule and ApiError.accessRuleErrors
 * (rule, field, position in the pattern).
 */
export interface AccessRuleService {
  /** The rules of a bucket (none when it has no rules) and their etag. */
  get(bucket: string): Promise<AccessRules>;
  /** Replaces every rule of the bucket; an empty list removes them. */
  set(bucket: string, rules: BucketAccessRule[], ifMatch?: string): Promise<AccessRules>;
  /**
   * Evaluates requests on keys with every source of the policy engine,
   * without performing them: the rules matching each key and the decision.
   */
  test(bucket: string, input: TestAccessRulesInput): Promise<AccessRulesTestResult>;
  /** Whether the bucket is public through its rules, and with which preset. */
  publicStatus(bucket: string): Promise<AccessRulesPublicStatus>;
  /**
   * Replaces the rules generated by the presets (ids preset-*) with those
   * of a preset; public-write needs input.confirm (400
   * confirmation_required otherwise).
   */
  applyPreset(bucket: string, input: AccessPresetInput, ifMatch?: string): Promise<AccessRules>;
  /** The block public access of a bucket, or of the cluster without bucket. */
  getBlockPublic(bucket?: string): Promise<BlockPublic>;
  /** Turns the block public access on or off; the mode is a cluster setting. */
  setBlockPublic(input: BlockPublicInput, bucket?: string): Promise<BlockPublic>;
}

/**
 * Distributions (D-DST-01..08, docs/10 §10.2): buckets served on custom
 * domains, their behaviors, the routing table and the signed URLs. A
 * change of a distribution reaches every node without restart. An alias
 * already served rejects with 409 alias_conflict, an invalid document
 * with 400 invalid_distribution; update with ifMatch rejects with 412
 * when the distribution changed since it was read.
 */
export interface DistributionService {
  /** Every distribution, sorted by id. */
  list(): Promise<Distribution[]>;
  get(id: string): Promise<Distribution>;
  /** Creates a distribution; origin secrets in clear are stored hashed. */
  create(distribution: Distribution): Promise<Distribution>;
  /** Replaces the whole document of distribution.id. */
  update(distribution: Distribution, ifMatch?: string): Promise<Distribution>;
  remove(id: string): Promise<void>;
  /** How the distribution serves a URL (behavior, key, headers), without reading the object. */
  test(id: string, input: DistributionTestInput): Promise<DistributionTestResult>;
  /** Generates a new origin secret, returned in clear this time only. */
  rotateOriginSecret(
    id: string,
    input?: RotateOriginSecretInput,
  ): Promise<RotateOriginSecretResult>;
  /** Signs a URL, or cookies, of a signed behavior with a private key of its key group. */
  sign(id: string, input: SignDistributionInput): Promise<SignDistributionResult>;
  /** What the routing table of the answering node serves for a host. */
  resolve(host: string): Promise<Route>;
  /**
   * The traffic of the aliases (of one distribution, or of all); null when
   * the server keeps no statistics.
   */
  traffic(distribution?: string): Promise<DistributionTraffic | null>;
}

/** Key groups of the signed behaviors (D-DST-05). */
export interface KeyGroupService {
  list(): Promise<KeyGroup[]>;
  get(id: string): Promise<KeyGroup>;
  /**
   * Creates a key group; with input.generateKey its first key pair is
   * generated and the private key is in the result this time only.
   */
  create(input: CreateKeyGroupInput): Promise<CreateKeyGroupResult>;
  /** Removes a key group no distribution references (409 otherwise). */
  remove(id: string): Promise<void>;
  /** The public keys of a key group, oldest first. */
  keys(id: string): Promise<PublicKey[]>;
  /**
   * Adds a public key (input.publicKeyPem) or a generated key pair, whose
   * private key is in the result this time only.
   */
  addKey(id: string, input: AddKeyInput): Promise<AddKeyResult>;
  removeKey(id: string, keyId: string): Promise<void>;
}

/** TLS certificates of the domains of the distributions (D-DST-07). */
export interface CertificateService {
  /** The uploaded and ACME certificates, with the ACME state. */
  list(): Promise<CertificateList>;
  get(id: string): Promise<Certificate>;
  /** Validates and stores a PEM certificate (400 invalid_certificate otherwise). */
  upload(input: UploadCertificateInput): Promise<Certificate>;
  /** Removes a certificate no distribution names (409 otherwise). */
  remove(id: string): Promise<void>;
  /** The DNS providers of the DNS-01 challenge, by name (never their credentials). */
  listDnsProviders(): Promise<DnsProvider[]>;
  /** 400 invalid_dns_provider, 409 dns_provider_exists. */
  createDnsProvider(input: DnsProviderInput): Promise<DnsProvider>;
  /** The empty credentials keep the stored ones. */
  updateDnsProvider(name: string, input: DnsProviderInput): Promise<DnsProvider>;
  /** 409 dns_provider_in_use while a distribution names it. */
  removeDnsProvider(name: string): Promise<void>;
  /** Publishes, checks and removes _acme-challenge.<domain>. */
  testDnsProvider(name: string, domain: string): Promise<DnsProviderTestResult>;
}

/**
 * Cache of the distributions (D-CCH-01..08, docs/11 §11.11). The
 * configuration (policies, settings of the cluster and of the
 * distributions, webhooks) and the diagnostics (inspector, statistics)
 * need admin:ManageCache; the operations on the cached content
 * (invalidations, warm-ups) need admin:InvalidateCache alone, so that a
 * deploy pipeline purges without changing the policies (TD-C-06).
 * Documents with an etag are replaced atomically: with ifMatch a
 * concurrent change rejects with 412 (ApiError.isPreconditionFailed); an
 * invalid document rejects with 400 invalid_cache_request, the field at
 * fault in the detail. Every change reaches every node without restart.
 */
export interface CacheService {
  /** The built-in policies (read-only) and the defined ones, by name. */
  listPolicies(): Promise<CachePolicy[]>;
  getPolicy(name: string): Promise<CachePolicy>;
  /** Defines a policy (409 already_exists, or builtin_cache_policy for a built-in name). */
  createPolicy(policy: CachePolicy): Promise<CachePolicy>;
  /** Replaces a defined policy; its entries in cache are dropped. */
  updatePolicy(policy: CachePolicy, ifMatch?: string): Promise<CachePolicy>;
  /** Removes a policy no level names (409 cache_policy_in_use otherwise). */
  deletePolicy(name: string): Promise<void>;
  /** Validates a policy without storing it: completed, its Cache-Control and a summary. */
  previewPolicy(policy: CachePolicy): Promise<CachePolicyPreview>;
  /** The settings of the cluster (global level; the debug secret masked). */
  getSettings(): Promise<CacheGlobalSettings>;
  updateSettings(settings: CacheGlobalSettings, ifMatch?: string): Promise<CacheGlobalSettings>;
  /** The settings of a distribution (empty without etag when it has none). */
  getDistribution(distribution: string): Promise<DistributionCache>;
  /** Replaces the settings of a distribution; masked webhook secrets keep the stored ones. */
  updateDistribution(config: DistributionCache, ifMatch?: string): Promise<DistributionCache>;
  /** The policy applying to a path of a domain, with the level of every setting. */
  effective(distribution: string, query?: CacheEffectiveQuery): Promise<CacheEffectivePolicy>;
  /** How the cache serves a URL: key, policy, bypass and the entry on every node. */
  inspect(input: CacheInspectInput): Promise<CacheInspection>;
  /** The statistics of the cluster and of every node, per domain, with the top objects. */
  stats(query?: CacheStatsQuery): Promise<CacheStats>;
  /**
   * Invalidates cached content of a distribution (paths, patterns, tags,
   * a domain, everything; soft marks the entries expired), or of every
   * distribution with distribution "" (request.all is required then).
   */
  invalidate(distribution: string, request: CacheInvalidationRequest): Promise<CacheInvalidation>;
  /** The history of a distribution, or of the global purges with "", newest first. */
  invalidations(distribution: string): Promise<CacheInvalidation[]>;
  /** An invalidation of a distribution, or found by its id alone with "". */
  invalidation(distribution: string, id: string): Promise<CacheInvalidation>;
  /** Starts a warm-up of a domain; resolves at once with the job. */
  warm(distribution: string, request: CacheWarmRequest): Promise<CacheWarmJob>;
  warmJob(distribution: string, id: string): Promise<CacheWarmJob>;
  /** The purge webhooks of a distribution (secrets masked). */
  webhooks(distribution: string): Promise<CacheWebhook[]>;
  /** Adds a purge webhook (409 already_exists for a used id). */
  addWebhook(distribution: string, webhook: CacheWebhook): Promise<CacheWebhook>;
  /** Removes a purge webhook; the domains bound to it are unbound. */
  removeWebhook(distribution: string, id: string): Promise<void>;
  /** Calls a webhook once (connection test); a failed call resolves with ok false. */
  testWebhook(distribution: string, id: string): Promise<CacheWebhookTestResult>;
}

/**
 * State of a live stream (Server-Sent Events): connecting (first
 * connection), open (receiving), reconnecting (the connection broke: it is
 * reopened automatically, with a growing delay) and closed (for good: by
 * close(), or by a refusal of the server such as 403, given as the error).
 */
export type StreamState = 'connecting' | 'open' | 'reconnecting' | 'closed';

/** The callbacks of a live stream. */
export interface StreamHandlers<T> {
  /** An item of the stream. */
  onItem(item: T): void;
  /**
   * The items the server dropped so far on this connection because the
   * client did not keep up (the server never waits for a slow stream).
   */
  onDropped?(total: number): void;
  /** The state changed; a stream closed by the server comes with its error. */
  onState?(state: StreamState, error?: ApiError): void;
}

/** A live stream; close() ends it (no callback is called afterwards). */
export interface LiveStream {
  close(): void;
}

/**
 * Monitoring of the deployment (D-MON-01..03, docs/07 §7.4), aggregated
 * over the nodes of a cluster: the series of the charts (one point every
 * 10 s for 24 h per node; admin:Prometheus or admin:ServerInfo), the live
 * trace of the requests (admin:ServerTrace) and the live logs
 * (admin:ConsoleLog) as Server-Sent Events, and the search of the audit
 * log (admin:ConsoleLog).
 */
export interface MonitoringService {
  /** The series available (GET /metrics). */
  metrics(): Promise<MetricInfo[]>;
  /** The points of a series per node and for the cluster (GET /metrics/query). */
  queryMetric(query: MetricQuery): Promise<MetricSeries>;
  /** Opens the live trace (GET /trace); it reconnects by itself until closed. */
  trace(filter: TraceFilter, handlers: StreamHandlers<TraceEvent>): LiveStream;
  /** Opens the live logs (GET /logs); it reconnects by itself until closed. */
  logs(filter: LogFilter, handlers: StreamHandlers<LogEntry>): LiveStream;
  /** Searches the recent audit records (GET /audit), newest first. */
  audit(query: AuditQuery): Promise<AuditEntry[]>;
}

export type Unsubscribe = () => void;

/**
 * Authentication events raised by the transport, independently of which
 * call failed. The session provider listens to them to drop the session
 * and send the user back to the login page (global 401 handling).
 */
export interface AuthEvents {
  onUnauthorized(listener: () => void): Unsubscribe;
}

/** The bundle of services handed to the React tree by ServicesProvider. */
export interface Services {
  readonly session: SessionService;
  readonly info: InfoService;
  readonly heal: HealService;
  readonly pools: PoolsService;
  readonly buckets: BucketService;
  readonly lifecycle: LifecycleService;
  readonly notifications: NotificationService;
  readonly media: MediaService;
  readonly replication: ReplicationService;
  readonly siteReplication: SiteReplicationService;
  readonly kms: KmsService;
  readonly objects: ObjectService;
  readonly uploads: UploadService;
  readonly users: UserService;
  readonly groups: GroupService;
  readonly policies: PolicyService;
  readonly accessKeys: AccessKeyService;
  readonly shares: ShareService;
  readonly config: ConfigService;
  readonly permissions: PermissionService;
  readonly accessRules: AccessRuleService;
  readonly distributions: DistributionService;
  readonly keyGroups: KeyGroupService;
  readonly certificates: CertificateService;
  readonly cache: CacheService;
  readonly monitoring: MonitoringService;
  readonly authEvents: AuthEvents;
}
