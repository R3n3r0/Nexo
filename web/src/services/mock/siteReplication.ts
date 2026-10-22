import type { SiteReplicationService } from '../interfaces';
import type { SiteReplicationInfo } from '../types';
import { clone, problem, type MockContext } from './support';

/**
 * SiteReplicationService in memory: the group is saved as given, the
 * deployment IDs derived from the names; the other sites are in sync at
 * once.
 */
export function createMockSiteReplicationService(ctx: MockContext): SiteReplicationService {
  const self = 'dep-console';
  let info: SiteReplicationInfo = {
    enabled: false,
    deploymentId: self,
    sites: [],
    peers: [],
    entries: 0,
  };
  return {
    async info() {
      await ctx.delay();
      return clone(info);
    },
    async add(sites) {
      await ctx.delay();
      if (info.enabled) {
        throw problem(
          409,
          'conflict',
          'this site is already in a site replication group: remove it first',
        );
      }
      if (sites.length < 2) {
        throw problem(400, 'invalid_argument', 'site replication needs at least two sites');
      }
      const names = new Set(sites.map((s) => s.name));
      if (names.size !== sites.length) {
        throw problem(400, 'invalid_argument', 'a site name is given twice');
      }
      const withIds = sites.map((s, i) => ({
        ...s,
        deploymentId: i === 0 ? self : `dep-${s.name}`,
      }));
      info = {
        enabled: true,
        name: sites[0]?.name,
        deploymentId: self,
        sites: [...withIds].sort((a, b) => a.name.localeCompare(b.name)),
        peers: withIds.slice(1).map((s) => ({
          name: s.name,
          deploymentId: s.deploymentId,
          lastSync: ctx.nowIso(),
          pushed: 0,
          pulled: 0,
          pending: 0,
        })),
        entries: 12,
      };
      return clone(info);
    },
    async remove() {
      await ctx.delay();
      if (!info.enabled) {
        throw problem(404, 'not_found', 'this site is in no site replication group');
      }
      info = { enabled: false, deploymentId: self, sites: [], peers: [], entries: 0 };
    },
    async sync() {
      await ctx.delay();
      if (!info.enabled) {
        throw problem(404, 'not_found', 'this site is in no site replication group');
      }
    },
  };
}
