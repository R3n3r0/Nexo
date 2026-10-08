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
  Bucket,
  CreateAccessKeyInput,
  CreateBucketInput,
  CreatedAccessKey,
  CreateGroupInput,
  CreateUserInput,
  Group,
  Policy,
  RemoveBucketOptions,
  SavePolicyInput,
  ServerInfo,
  Session,
  User,
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
}

/** Bucket management. */
export interface BucketService {
  list(): Promise<Bucket[]>;
  get(name: string): Promise<Bucket>;
  create(input: CreateBucketInput): Promise<Bucket>;
  remove(name: string, options?: RemoveBucketOptions): Promise<void>;
}

/** IAM users. */
export interface UserService {
  list(): Promise<User[]>;
  create(input: CreateUserInput): Promise<User>;
  remove(accessKey: string): Promise<void>;
}

/** IAM groups. */
export interface GroupService {
  list(): Promise<Group[]>;
  create(input: CreateGroupInput): Promise<Group>;
  remove(name: string): Promise<void>;
}

/** IAM policies. */
export interface PolicyService {
  list(): Promise<Policy[]>;
  get(name: string): Promise<Policy>;
  save(input: SavePolicyInput): Promise<Policy>;
  remove(name: string): Promise<void>;
}

/** Service accounts (access keys derived from a user). */
export interface AccessKeyService {
  list(): Promise<AccessKey[]>;
  create(input: CreateAccessKeyInput): Promise<CreatedAccessKey>;
  remove(accessKey: string): Promise<void>;
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
  readonly users: UserService;
  readonly groups: GroupService;
  readonly policies: PolicyService;
  readonly accessKeys: AccessKeyService;
  readonly authEvents: AuthEvents;
}
