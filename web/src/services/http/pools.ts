import type { PoolsService } from '../interfaces';
import type { PoolPlan, Pools } from '../types';
import type { HttpClient } from './client';

/** PoolsService over GET /pools, POST /pools/plan, /pools/{pool}/decommission and /rebalance. */
export function createHttpPoolsService(client: HttpClient): PoolsService {
  const decommission = (pool: number) => `/pools/${String(pool)}/decommission`;
  return {
    list() {
      return client.request<Pools>('/pools');
    },
    decommission(pool) {
      return client.request<void>(decommission(pool), { method: 'POST' });
    },
    cancelDecommission(pool) {
      return client.request<void>(decommission(pool), { method: 'DELETE' });
    },
    startRebalance() {
      return client.request<void>('/rebalance', { method: 'POST' });
    },
    stopRebalance() {
      return client.request<void>('/rebalance', { method: 'DELETE' });
    },
    plan(input) {
      return client.request<PoolPlan>('/pools/plan', { method: 'POST', body: input });
    },
  };
}
