import type { HealService } from '../interfaces';
import type { HealJob } from '../types';
import type { HttpClient } from './client';
import { collectPages, segment } from './pages';

/** HealService over POST /heal, GET /heal and GET /heal/{id}. */
export function createHttpHealService(client: HttpClient): HealService {
  return {
    start(request) {
      return client.request<HealJob>('/heal', { method: 'POST', body: request });
    },
    list() {
      return collectPages<HealJob>(client, '/heal');
    },
    get(id) {
      return client.request<HealJob>(`/heal/${segment(id)}`);
    },
  };
}
