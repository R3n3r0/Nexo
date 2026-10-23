import { isApiError } from '../errors';
import type { MaintenanceService } from '../interfaces';
import type { UpdateJob } from '../types';
import type { HttpClient } from './client';

/** MaintenanceService over /service/update and /service/cluster/{restart,stop}. */
export function createHttpMaintenanceService(client: HttpClient): MaintenanceService {
  return {
    async job() {
      try {
        return await client.request<UpdateJob>('/service/update');
      } catch (err) {
        if (isApiError(err) && err.status === 404) return null;
        throw err;
      }
    },
    update(input) {
      return client.request<UpdateJob>('/service/update', { method: 'POST', body: input });
    },
    restart(mode) {
      return client.request<UpdateJob>('/service/cluster/restart', {
        method: 'POST',
        body: { mode },
      });
    },
    async stop() {
      await client.request<unknown>('/service/cluster/stop', { method: 'POST' });
    },
  };
}
