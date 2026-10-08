import type { InfoService } from '../interfaces';
import type { BucketUsage, DataUsage, ServerInfo } from '../types';
import type { MockStorage } from './storage';
import { clone, type MockContext } from './support';

/** A single-node, single-drive server as the demo baseline. */
export function sampleServerInfo(overrides: Partial<ServerInfo> = {}): ServerInfo {
  return {
    version: '0.1.0-dev',
    commit: 'abc1234',
    buildDate: '2026-10-07T00:00:00Z',
    mode: 'snsd',
    region: 'us-east-1',
    deploymentId: 'f3b1c9d4-7e2a-4c8e-9a1b-0d2e3f4a5b6c',
    startedAt: '2026-10-07T08:00:00Z',
    uptimeSeconds: 3 * 86400 + 4 * 3600 + 12 * 60,
    pools: [{ index: 0, sets: 1, drivesPerSet: 1, parity: 0 }],
    nodes: [
      {
        endpoint: 'http://localhost:9000',
        state: 'online',
        version: '0.1.0-dev',
        commit: 'abc1234',
        uptimeSeconds: 3 * 86400 + 4 * 3600 + 12 * 60,
        disks: [
          {
            endpoint: 'http://localhost:9000',
            path: '/data',
            id: 'disk-0',
            poolIdx: 0,
            setIdx: 0,
            diskIdx: 0,
            state: 'online',
            total: 512 * 1024 ** 3,
            free: 318 * 1024 ** 3,
            used: 194 * 1024 ** 3,
            healing: false,
          },
        ],
      },
    ],
    buckets: 3,
    objects: 12840,
    usage: 194 * 1024 ** 3,
    ...overrides,
  };
}

export interface MockInfoOptions {
  info?: ServerInfo;
}

export function createMockInfoService(
  ctx: MockContext,
  options: MockInfoOptions = {},
  storage?: MockStorage,
): InfoService {
  const info = options.info ?? sampleServerInfo();
  return {
    async serverInfo() {
      await ctx.delay();
      return clone(info);
    },
    async usage() {
      await ctx.delay();
      const buckets: Record<string, BucketUsage> = {};
      let objects = 0;
      let versions = 0;
      let size = 0;
      for (const { bucket } of storage?.buckets.values() ?? []) {
        buckets[bucket.name] = {
          size: bucket.size,
          objectsCount: bucket.objects,
          versionsCount: bucket.versions,
          deleteMarkers: bucket.deleteMarkers ?? 0,
        };
        objects += bucket.objects;
        versions += bucket.versions;
        size += bucket.size;
      }
      const usage: DataUsage = {
        lastUpdate: ctx.nowIso(),
        objectsCount: objects,
        versionsCount: versions,
        totalSize: size,
        bucketsCount: Object.keys(buckets).length,
        buckets,
      };
      return usage;
    },
  };
}
