import type { TierService } from '../interfaces';
import type { Tier } from '../types';
import { clone, problem, type MockContext } from './support';

/**
 * TierService in memory: the tiers are saved as given, without their
 * secret; a tier named HOLDING holds data and cannot be removed.
 */
export function createMockTierService(ctx: MockContext): TierService {
  let tiers: Tier[] = [];
  const find = (name: string) => tiers.findIndex((t) => t.name === name);
  return {
    async list() {
      await ctx.delay();
      return clone(tiers);
    },
    async add(input) {
      await ctx.delay();
      const name = input.name ?? '';
      if (
        !/^[A-Z0-9_-]{1,64}$/.test(name) ||
        name === 'STANDARD' ||
        name === 'REDUCED_REDUNDANCY'
      ) {
        throw problem(400, 'invalid_argument', `invalid tier name "${name}"`);
      }
      if (find(name) >= 0) {
        throw problem(409, 'conflict', `tier ${name} exists`);
      }
      if (!input.endpoint || !input.bucket || !input.accessKey || !input.secretKey) {
        throw problem(
          400,
          'invalid_argument',
          'the endpoint, the bucket and the credentials are required',
        );
      }
      const now = ctx.nowIso();
      const tier: Tier = {
        name,
        endpoint: input.endpoint.replace(/\/+$/, ''),
        bucket: input.bucket,
        prefix: input.prefix || undefined,
        region: input.region || undefined,
        storageClass: input.storageClass || undefined,
        accessKey: input.accessKey,
        created: now,
        updated: now,
      };
      tiers = [...tiers, tier].sort((a, b) => a.name.localeCompare(b.name));
      return clone(tier);
    },
    async edit(name, input) {
      await ctx.delay();
      const i = find(name);
      if (i < 0) {
        throw problem(404, 'not_found', `tier ${name}`);
      }
      const cur = tiers[i] as Tier;
      const next: Tier = {
        ...cur,
        accessKey: input.accessKey,
        region: input.region || cur.region,
        storageClass: input.storageClass || cur.storageClass,
        updated: ctx.nowIso(),
      };
      tiers = tiers.map((t) => (t.name === name ? next : t));
      return clone(next);
    },
    async remove(name) {
      await ctx.delay();
      if (find(name) < 0) {
        throw problem(404, 'not_found', `tier ${name}`);
      }
      if (name === 'HOLDING') {
        throw problem(409, 'conflict', `tier ${name} holds the data of transitioned versions`);
      }
      tiers = tiers.filter((t) => t.name !== name);
    },
  };
}
