import { isApiError } from '../errors';
import type { CertificateService, DistributionService, KeyGroupService } from '../interfaces';
import type {
  AddKeyResult,
  Certificate,
  CertificateList,
  CreateKeyGroupResult,
  Distribution,
  DistributionTestResult,
  DistributionTraffic,
  HostTraffic,
  KeyGroup,
  PublicKey,
  RotateOriginSecretResult,
  Route,
  SignDistributionResult,
} from '../types';
import { ifMatchHeader } from './accessRules';
import type { HttpClient } from './client';
import type { components } from './schema';

interface Items<T> {
  items: T[];
}

/** DistributionService over /distributions, /routing/resolve and the cache statistics. */
export function createHttpDistributionService(client: HttpClient): DistributionService {
  const path = (id: string, sub = '') =>
    `/distributions/${encodeURIComponent(id)}${sub === '' ? '' : `/${sub}`}`;
  return {
    async list() {
      const page = await client.request<Items<Distribution>>('/distributions');
      return page.items;
    },
    get(id) {
      return client.request<Distribution>(path(id));
    },
    create(distribution) {
      return client.request<Distribution>('/distributions', {
        method: 'POST',
        body: distribution,
      });
    },
    update(distribution, ifMatch) {
      return client.request<Distribution>(path(distribution.id), {
        method: 'PUT',
        body: distribution,
        headers: ifMatchHeader(ifMatch),
      });
    },
    remove(id) {
      return client.request<void>(path(id), { method: 'DELETE' });
    },
    test(id, input) {
      return client.request<DistributionTestResult>(path(id, 'test'), {
        method: 'POST',
        body: input,
      });
    },
    rotateOriginSecret(id, input = {}) {
      return client.request<RotateOriginSecretResult>(path(id, 'origin-secret/rotate'), {
        method: 'POST',
        body: input,
      });
    },
    sign(id, input) {
      return client.request<SignDistributionResult>(path(id, 'sign'), {
        method: 'POST',
        body: input,
      });
    },
    resolve(host) {
      return client.request<Route>('/routing/resolve', { query: { host } });
    },
    async traffic(distribution) {
      try {
        const stats = await client.request<components['schemas']['CacheStats']>('/cache/stats', {
          query: { distribution: distribution === '' ? undefined : distribution },
        });
        return trafficOf(stats);
      } catch (error) {
        // A server without the cache API (404, 501), or a caller who may
        // not read it (403), has no statistics to show.
        if (isApiError(error) && [403, 404, 405, 501].includes(error.status)) {
          return null;
        }
        throw error;
      }
    },
  };
}

/** A number, or the sum of the numbers of a record (requests by cache result). */
function total(value: unknown): number {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  if (typeof value === 'object' && value !== null) {
    return Object.values(value as Record<string, unknown>).reduce<number>(
      (sum, v) => sum + (typeof v === 'number' && Number.isFinite(v) ? v : 0),
      0,
    );
  }
  return 0;
}

/**
 * The per-alias counters of a cache statistics report (schema CacheStats,
 * docs/11 §11.8: hosts[] with the requests by X-Cache value, bytesCache,
 * bytesOrigin, hitRatio), read defensively; null when the document has no
 * per-host counters.
 */
export function trafficOf(stats: unknown): DistributionTraffic | null {
  if (typeof stats !== 'object' || stats === null) {
    return null;
  }
  const hosts = (stats as { hosts?: unknown }).hosts;
  if (hosts !== undefined && !Array.isArray(hosts)) {
    return null;
  }
  const out: HostTraffic[] = [];
  for (const h of (hosts ?? []) as unknown[]) {
    if (typeof h !== 'object' || h === null) continue;
    const r = h as Record<string, unknown>;
    if (typeof r.host !== 'string') continue;
    out.push({
      distribution: typeof r.distribution === 'string' ? r.distribution : '',
      host: r.host,
      requests: total(r.requests),
      bytes: total(r.bytesCache) + total(r.bytesOrigin) + total(r.bytes),
      hitRatio: typeof r.hitRatio === 'number' ? r.hitRatio : 0,
    });
  }
  return { hosts: out };
}

/** KeyGroupService over /keygroups. */
export function createHttpKeyGroupService(client: HttpClient): KeyGroupService {
  const path = (id: string, sub = '') =>
    `/keygroups/${encodeURIComponent(id)}${sub === '' ? '' : `/${sub}`}`;
  return {
    async list() {
      const page = await client.request<Items<KeyGroup>>('/keygroups');
      return page.items;
    },
    get(id) {
      return client.request<KeyGroup>(path(id));
    },
    create(input) {
      return client.request<CreateKeyGroupResult>('/keygroups', { method: 'POST', body: input });
    },
    remove(id) {
      return client.request<void>(path(id), { method: 'DELETE' });
    },
    async keys(id) {
      const page = await client.request<Items<PublicKey>>(path(id, 'keys'));
      return page.items;
    },
    addKey(id, input) {
      return client.request<AddKeyResult>(path(id, 'keys'), { method: 'POST', body: input });
    },
    removeKey(id, keyId) {
      return client.request<void>(path(id, `keys/${encodeURIComponent(keyId)}`), {
        method: 'DELETE',
      });
    },
  };
}

/** CertificateService over /certificates. */
export function createHttpCertificateService(client: HttpClient): CertificateService {
  const path = (id: string) => `/certificates/${encodeURIComponent(id)}`;
  return {
    list() {
      return client.request<CertificateList>('/certificates');
    },
    get(id) {
      return client.request<Certificate>(path(id));
    },
    upload(input) {
      return client.request<Certificate>('/certificates', { method: 'POST', body: input });
    },
    remove(id) {
      return client.request<void>(path(id), { method: 'DELETE' });
    },
  };
}
