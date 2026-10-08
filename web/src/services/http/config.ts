import type { ConfigService } from '../interfaces';
import type { ConfigDocument, ConfigSubsystem, ConfigSubsystemSchema } from '../types';
import type { HttpClient } from './client';
import { segment } from './pages';

/** ConfigService over /config, /config/schema, /config/{export,import} and /config/{subsys}. */
export function createHttpConfigService(client: HttpClient): ConfigService {
  return {
    list() {
      return client.request<ConfigSubsystem[]>('/config');
    },
    schema() {
      return client.request<ConfigSubsystemSchema[]>('/config/schema');
    },
    get(subsystem) {
      return client.request<ConfigSubsystem>(`/config/${segment(subsystem)}`);
    },
    set(subsystem, settings) {
      return client.request<ConfigSubsystem>(`/config/${segment(subsystem)}`, {
        method: 'PUT',
        body: { settings },
      });
    },
    reset(subsystem, keys) {
      return client.request<void>(`/config/${segment(subsystem)}`, {
        method: 'DELETE',
        query: { keys: keys !== undefined && keys.length > 0 ? keys.join(',') : undefined },
      });
    },
    exportAll() {
      return client.request<ConfigDocument>('/config/export');
    },
    importAll(document) {
      return client.request<void>('/config/import', { method: 'POST', body: document });
    },
  };
}
