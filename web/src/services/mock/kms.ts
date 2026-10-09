import type { KmsService } from '../interfaces';
import type { MockStorage } from './storage';
import { clone, problem } from './support';

/** The KMS of the mock deployment (MockStorageOptions.kms). */
export function createMockKmsService(storage: MockStorage): KmsService {
  const { ctx, kms } = storage;

  function configured() {
    if (!kms.status.configured) {
      throw problem(501, 'not_implemented', 'No KMS is configured.');
    }
  }

  return {
    async status() {
      await ctx.delay();
      return clone(kms.status);
    },
    async keys(prefix) {
      await ctx.delay();
      configured();
      return clone(kms.keys.filter((k) => k.id.startsWith(prefix ?? '')));
    },
    async createKey(id) {
      await ctx.delay();
      configured();
      if (!/^[A-Za-z0-9_.-]{1,80}$/.test(id)) {
        throw problem(400, 'invalid_argument', `Key name ${id} is not valid.`);
      }
      if (kms.keys.some((k) => k.id === id)) {
        throw problem(409, 'conflict', `Key ${id} already exists.`);
      }
      const key = { id, createdAt: ctx.nowIso() };
      kms.keys.push(key);
      return clone(key);
    },
  };
}
