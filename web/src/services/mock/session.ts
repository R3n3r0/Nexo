import type { AuthEventEmitter } from '../events';
import type { SessionService } from '../interfaces';
import type { Session } from '../types';
import { accessDenied, clone, unauthorized, type MockContext } from './support';

export interface MockSessionOptions {
  /** Accepted credentials: access key -> secret key. */
  accounts: Record<string, string>;
  /** Session lifetime in seconds. */
  ttlSeconds?: number;
  /** Start already authenticated as this account. */
  initialAccessKey?: string;
  /** When set, a current() call without a session also raises the global event. */
  authEvents?: AuthEventEmitter;
}

/** SessionService with an in-memory session; login checks the configured accounts. */
export interface MockSessionService extends SessionService {
  /** Drops the session without going through logout (simulates expiry). */
  expire(): void;
}

export function createMockSessionService(
  ctx: MockContext,
  options: MockSessionOptions,
): MockSessionService {
  const ttl = options.ttlSeconds ?? 12 * 3600;
  let session: Session | null = null;
  let counter = 0;

  function open(accessKey: string): Session {
    counter += 1;
    const issued = ctx.now();
    const expires = new Date(issued.getTime() + ttl * 1000);
    return {
      id: `mock-session-${counter}`,
      accessKey,
      identity: {
        accessKey,
        isRoot: accessKey === Object.keys(options.accounts)[0],
        groups: [],
        policyNames: ['consoleAdmin'],
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
      const expected = Object.hasOwn(options.accounts, accessKey)
        ? options.accounts[accessKey]
        : undefined;
      if (expected === undefined || expected !== secretKey) {
        throw accessDenied();
      }
      session = open(accessKey);
      return clone(session);
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
  };
}
