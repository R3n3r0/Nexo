import { ApiError } from '../errors';

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
export function problem(status: number, code: string, detail: string): ApiError {
  return new ApiError({ title: statusTitle(status), status, code, detail });
}

export const unauthorized = (): ApiError =>
  problem(401, 'invalid_access_key', 'Authentication required.');

export const accessDenied = (): ApiError =>
  problem(403, 'access_denied', 'The credentials are not valid.');

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
    case 409:
      return 'Conflict';
    default:
      return `HTTP ${status}`;
  }
}

/** Deep copy to keep callers from mutating the mock store. */
export function clone<T>(value: T): T {
  return structuredClone(value);
}
