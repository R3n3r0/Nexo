import type { AuthEventEmitter } from '../events';
import type { SessionService } from '../interfaces';
import type { AuthProviders, Session } from '../types';
import {
  clone,
  invalidCredentials,
  tooManyRequests,
  unauthorized,
  type MockContext,
} from './support';

export interface MockSessionOptions {
  /** Accepted credentials: access key -> secret key. */
  accounts: Record<string, string>;
  /** Session lifetime in seconds. */
  ttlSeconds?: number;
  /** Start already authenticated as this account. */
  initialAccessKey?: string;
  /** When set, a current() call without a session also raises the global event. */
  authEvents?: AuthEventEmitter;
  /** Groups and effective policies of a principal (from the IAM mock). */
  identityOf?: (accessKey: string) => { groups: string[]; policyNames: string[] };
  /** Accepts other credentials (users created in the IAM mock). */
  verify?: (accessKey: string, secretKey: string) => boolean;
  /** Identity providers of the login page (default: none). */
  providers?: AuthProviders;
  /** LDAP users: username -> password (with providers.ldap). */
  ldapAccounts?: Record<string, string>;
}

/** SessionService with an in-memory session; login checks the configured accounts. */
export interface MockSessionService extends SessionService {
  /** Drops the session without going through logout (simulates expiry). */
  expire(): void;
  /** Access key of the open session, null without one (pure accessor). */
  currentAccessKey(): string | null;
}

/** Failed logins of an access key before the lockout, like the Go session service. */
const maxLoginFailures = 5;
/** Lockout after too many failures, in milliseconds. */
const loginLockoutMs = 60_000;

export function createMockSessionService(
  ctx: MockContext,
  options: MockSessionOptions,
): MockSessionService {
  const ttl = options.ttlSeconds ?? 12 * 3600;
  let session: Session | null = null;
  let counter = 0;
  // Failed logins per access key (the mock has a single client).
  const failures = new Map<string, { count: number; lockedUntil: number }>();

  function open(accessKey: string, federated?: { provider: string; username: string }): Session {
    counter += 1;
    const issued = ctx.now();
    const expires = new Date(issued.getTime() + ttl * 1000);
    const isRoot = accessKey === Object.keys(options.accounts)[0];
    const iam = options.identityOf?.(accessKey);
    return {
      id: `mock-session-${counter}`,
      accessKey,
      identity: {
        accessKey,
        isRoot: federated === undefined && isRoot,
        ...(federated ?? {}),
        groups: iam?.groups ?? [],
        policyNames: iam !== undefined && !isRoot ? iam.policyNames : ['consoleAdmin'],
      },
      issuedAt: issued.toISOString(),
      expiresAt: expires.toISOString(),
      csrfToken: `mock-csrf-${counter}`,
    };
  }

  if (options.initialAccessKey !== undefined) {
    session = open(options.initialAccessKey);
  }

  return {
    async login(accessKey, secretKey) {
      await ctx.delay();
      const now = ctx.now().getTime();
      const failed = failures.get(accessKey);
      if (failed !== undefined && failed.lockedUntil > now) {
        throw tooManyRequests();
      }
      const expected = Object.hasOwn(options.accounts, accessKey)
        ? options.accounts[accessKey]
        : undefined;
      const accepted =
        expected !== undefined
          ? expected === secretKey
          : (options.verify?.(accessKey, secretKey) ?? false);
      if (!accepted) {
        const count = (failed !== undefined && failed.lockedUntil === 0 ? failed.count : 0) + 1;
        failures.set(accessKey, {
          count,
          lockedUntil: count >= maxLoginFailures ? now + loginLockoutMs : 0,
        });
        throw invalidCredentials();
      }
      failures.delete(accessKey);
      session = open(accessKey);
      return clone(session);
    },
    async loginLDAP(username, password) {
      await ctx.delay();
      const key = `ldap:${username}`;
      const now = ctx.now().getTime();
      const failed = failures.get(key);
      if (failed !== undefined && failed.lockedUntil > now) {
        throw tooManyRequests();
      }
      const expected =
        options.providers?.ldap === true && options.ldapAccounts !== undefined
          ? options.ldapAccounts[username]
          : undefined;
      if (expected === undefined || password === '' || expected !== password) {
        const count = (failed !== undefined && failed.lockedUntil === 0 ? failed.count : 0) + 1;
        failures.set(key, {
          count,
          lockedUntil: count >= maxLoginFailures ? now + loginLockoutMs : 0,
        });
        throw invalidCredentials();
      }
      failures.delete(key);
      counter += 1;
      session = open(`FEDERATED${String(counter).padStart(11, '0')}`, {
        provider: 'ldap',
        username,
      });
      return clone(session);
    },
    async providers() {
      await ctx.delay();
      return { openid: false, ldap: false, ...options.providers };
    },
    openIDLoginURL() {
      return '/api/v1/auth/openid/start';
    },
    async logout() {
      await ctx.delay();
      session = null;
    },
    async current() {
      await ctx.delay();
      if (session === null) {
        options.authEvents?.emitUnauthorized();
        throw unauthorized();
      }
      return clone(session);
    },
    expire() {
      session = null;
    },
    currentAccessKey() {
      return session?.accessKey ?? null;
    },
  };
}
