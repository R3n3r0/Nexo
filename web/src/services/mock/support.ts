import { ApiError, type ProblemDetails } from '../errors';

/** Options shared by every mock service. */
export interface MockBehaviour {
  /** Artificial latency applied to every call (demo mode); 0 in tests. */
  latencyMs?: number;
  /** Clock used for generated timestamps. */
  now?: () => Date;
}

export interface MockContext {
  delay(): Promise<void>;
  now(): Date;
  nowIso(): string;
}

export function createMockContext(behaviour: MockBehaviour = {}): MockContext {
  const latencyMs = behaviour.latencyMs ?? 0;
  const now = behaviour.now ?? (() => new Date());
  return {
    delay() {
      if (latencyMs <= 0) {
        return Promise.resolve();
      }
      return new Promise((resolve) => {
        setTimeout(resolve, latencyMs);
      });
    },
    now,
    nowIso() {
      return now().toISOString();
    },
  };
}

/** Builds an ApiError the way the Go transport would for a sentinel error. */
export function problem(
  status: number,
  code: string,
  detail: string,
  extra: Pick<ProblemDetails, 'errors' | 'ruleErrors'> = {},
): ApiError {
  return new ApiError({ title: statusTitle(status), status, code, detail, ...extra });
}

/** No valid session (the Go session middleware: 401 unauthenticated). */
export const unauthorized = (): ApiError =>
  problem(401, 'unauthenticated', 'Authentication required.');

/** Wrong credentials at the login (POST /auth/login: 401 invalid_credentials). */
export const invalidCredentials = (): ApiError =>
  problem(401, 'invalid_credentials', 'invalid access key or secret key');

/** A login refused after too many failures (POST /auth/login: 429). */
export const tooManyRequests = (): ApiError =>
  problem(429, 'too_many_requests', 'too many failed login attempts, retry later');

function statusTitle(status: number): string {
  switch (status) {
    case 400:
      return 'Bad Request';
    case 401:
      return 'Unauthorized';
    case 403:
      return 'Forbidden';
    case 404:
      return 'Not Found';
    case 405:
      return 'Method Not Allowed';
    case 409:
      return 'Conflict';
    case 429:
      return 'Too Many Requests';
    case 501:
      return 'Not Implemented';
    default:
      return `HTTP ${status}`;
  }
}

/**
 * The invalid_configuration problem of a bucket configuration (lifecycle,
 * subscriptions, CORS) with its invalid rules, like the Admin API.
 */
export function invalidConfiguration(
  ruleErrors: NonNullable<ProblemDetails['ruleErrors']>,
): ApiError {
  return problem(
    400,
    'invalid_configuration',
    ruleErrors.length === 1
      ? (ruleErrors[0]?.message ?? 'The configuration is not valid.')
      : `${String(ruleErrors.length)} rules are not valid.`,
    { ruleErrors },
  );
}

/** Deep copy to keep callers from mutating the mock store. */
export function clone<T>(value: T): T {
  return structuredClone(value);
}
