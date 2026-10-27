import type { TierService } from '../interfaces';
import type { Tier } from '../types';
import type { HttpClient } from './client';

/** TierService over /tiers. */
export function createHttpTierService(client: HttpClient): TierService {
  return {
    async list() {
      return (await client.request<{ tiers: Tier[] }>('/tiers')).tiers;
    },
    add(input) {
      return client.request<Tier>('/tiers', { method: 'POST', body: input });
    },
    edit(name, input) {
      return client.request<Tier>(`/tiers/${encodeURIComponent(name)}`, {
        method: 'PUT',
        body: input,
      });
    },
    remove(name) {
      return client.request<void>(`/tiers/${encodeURIComponent(name)}`, { method: 'DELETE' });
    },
  };
}
