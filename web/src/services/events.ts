import type { AuthEvents, Unsubscribe } from './interfaces';

/** AuthEvents plus the emitting side, used by the implementations. */
export interface AuthEventEmitter extends AuthEvents {
  emitUnauthorized(): void;
}

/** Creates an in-memory emitter shared by a bundle of services. */
export function createAuthEventEmitter(): AuthEventEmitter {
  const listeners = new Set<() => void>();
  return {
    onUnauthorized(listener): Unsubscribe {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    emitUnauthorized(): void {
      // Copy first: a listener may unsubscribe while being notified.
      for (const listener of [...listeners]) {
        listener();
      }
    },
  };
}
