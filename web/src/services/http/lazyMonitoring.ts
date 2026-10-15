import { ApiError } from '../errors';
import type { LiveStream, MonitoringService } from '../interfaces';
import type { HttpClient } from './client';
import type { HttpMonitoringOptions } from './monitoring';

/**
 * The MonitoringService over the Admin API, its implementation (the
 * Server-Sent Events streams) loaded on first use: only the monitoring
 * pages need it, so it stays out of the initial bundle of the console
 * (docs/06 §6.5). A stream opened before the module is loaded opens as
 * soon as it is; closed in the meantime, it never opens.
 */
export function createLazyHttpMonitoringService(
  client: HttpClient,
  options: HttpMonitoringOptions = {},
): MonitoringService {
  let loaded: Promise<MonitoringService> | undefined;
  const load = () => {
    loaded ??= import('./monitoring').then(
      (module) => module.createHttpMonitoringService(client, options),
      (cause: unknown) => {
        loaded = undefined;
        throw ApiError.network(cause);
      },
    );
    return loaded;
  };
  const stream = (
    open: (service: MonitoringService) => LiveStream,
    onError: ((error: ApiError) => void) | undefined,
  ): LiveStream => {
    let inner: LiveStream | undefined;
    let closed = false;
    load().then(
      (service) => {
        if (!closed) inner = open(service);
      },
      (error: unknown) => {
        if (!closed) onError?.(error instanceof ApiError ? error : ApiError.network(error));
      },
    );
    return {
      close() {
        closed = true;
        inner?.close();
      },
    };
  };
  return {
    async metrics() {
      return (await load()).metrics();
    },
    async queryMetric(query) {
      return (await load()).queryMetric(query);
    },
    async audit(query) {
      return (await load()).audit(query);
    },
    trace(filter, handlers) {
      return stream(
        (service) => service.trace(filter, handlers),
        (error) => handlers.onState?.('closed', error),
      );
    },
    logs(filter, handlers) {
      return stream(
        (service) => service.logs(filter, handlers),
        (error) => handlers.onState?.('closed', error),
      );
    },
  };
}
