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
  AccessKeyFilter,
  AttachPolicyInput,
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
  KmsKey,
  KmsStatus,
  LegalHoldStatus,
  LifecycleApplyResult,
  LifecycleConfiguration,
  ListObjectsQuery,
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

/** Console session (cookie + CSRF token managed by the implementation). */
export interface SessionService {
  /** Authenticates with an access/secret key pair and opens a session. */
  login(accessKey: string, secretKey: string): Promise<Session>;
  /** Closes the current session; never fails when no session exists. */
  logout(): Promise<void>;
  /** Returns the current session or rejects with a 401 ApiError. */
  current(): Promise<Session>;
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
  readonly buckets: BucketService;
  readonly lifecycle: LifecycleService;
  readonly notifications: NotificationService;
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
  readonly authEvents: AuthEvents;
}
