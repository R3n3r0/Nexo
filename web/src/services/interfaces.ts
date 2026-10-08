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
  Bucket,
  BucketAccess,
  BucketEncryption,
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
  CreateUploadInput,
  CreateUserInput,
  DataUsage,
  DeleteObjectsInput,
  DeleteObjectsResult,
  Group,
  ListObjectsQuery,
  ObjectDetails,
  ObjectEntry,
  ObjectList,
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
  Share,
  ShareFilter,
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
  /** Default encryption; null when the bucket has none. */
  getEncryption(name: string): Promise<BucketEncryption | null>;
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
  /** Starts a multipart upload; the answer carries the part size limits. */
  create(bucket: string, key: string, input?: CreateUploadInput): Promise<Upload>;
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
  readonly buckets: BucketService;
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
