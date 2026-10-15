import { ApiError } from '../errors';

/**
 * A service whose implementation is loaded on first use, for the services
 * that only the pages loaded on demand need (cache, distributions): their
 * code stays out of the initial bundle of the console (docs/06 §6.5). Every
 * method of such a service returns a promise, so that the proxy can wait
 * for the module; a failed load rejects with a network ApiError and is
 * retried by the next call.
 */
export function lazyService<T extends object>(load: () => Promise<T>): T {
  let loaded: Promise<T> | undefined;
  const get = () => {
    loaded ??= load().catch((cause: unknown) => {
      loaded = undefined;
      throw ApiError.network(cause);
    });
    return loaded;
  };
  return new Proxy({} as T, {
    get(_target, name) {
      // Not a thenable, nor anything a symbol lookup expects a method for.
      if (typeof name !== 'string' || name === 'then') return undefined;
      return async (...args: unknown[]) => {
        const service = await get();
        const method: unknown = Reflect.get(service, name);
        if (typeof method !== 'function') {
          throw new TypeError(`${name} is not a method of the service`);
        }
        return (method as (...a: unknown[]) => unknown).apply(service, args);
      };
    },
  });
}
