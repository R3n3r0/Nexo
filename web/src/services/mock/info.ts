import type { InfoService } from '../interfaces';
import type { BucketUsage, DataUsage, DiskInfo, NodeInfo, ServerInfo } from '../types';
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
    pools: [
      {
        index: 0,
        sets: 1,
        drivesPerSet: 1,
        parity: 0,
        readQuorum: 1,
        writeQuorum: 1,
        erasureSets: [{ index: 0, drives: 1, online: 1, offline: 0, healing: 0, state: 'ok' }],
      },
    ],
    nodes: [
      {
        endpoint: 'http://localhost:9000',
        state: 'online',
        version: '0.1.0-dev',
        commit: 'abc1234',
        local: true,
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
    tls: {
      enabled: true,
      minVersion: '1.2',
      internodeVerify: false,
      certificates: [
        {
          name: '',
          domains: ['nexo.example.com', 'localhost'],
          ips: ['127.0.0.1'],
          subject: 'CN=nexo.example.com',
          issuer: 'CN=Example CA',
          notBefore: '2026-07-01T00:00:00Z',
          notAfter: '2027-07-01T00:00:00Z',
          fingerprint: '4f3c2a9e1b7d6c5a8e0f1d2c3b4a5968778695a4b3c2d1e0f9e8d7c6b5a49382',
        },
      ],
    },
    telemetry: { enabled: false, sampleRatio: 1 },
    ...overrides,
  };
}

const GiB = 1024 ** 3;

/** A drive of the sample cluster: 100 GiB, partly used. */
function clusterDisk(node: number, disk: number, state: DiskInfo['state']): DiskInfo {
  const used = (10 + node * 3 + disk) * GiB;
  return {
    endpoint: `http://nexo${String(node)}:9000/data${String(disk)}`,
    path: `/data${String(disk)}`,
    id: `disk-${String(node)}-${String(disk)}`,
    poolIdx: 0,
    setIdx: 0,
    diskIdx: (node - 1) * 2 + disk - 1,
    state,
    total: state === 'offline' ? 0 : 100 * GiB,
    used: state === 'offline' ? 0 : used,
    free: state === 'offline' ? 0 : 100 * GiB - used,
    healing: state === 'healing',
    ...(state === 'offline' ? { error: 'node offline' } : {}),
  };
}

/**
 * A distributed deployment of 4 nodes of 2 drives (one erasure set of 8,
 * EC:4) seen from nexo1: nexo2 is offline, a drive of nexo3 heals and the
 * clock of nexo4 drifts. Used by the tests of the cluster pages.
 */
export function sampleClusterInfo(overrides: Partial<ServerInfo> = {}): ServerInfo {
  const uptime = 2 * 86400;
  const node = (n: number, extra: Partial<NodeInfo>): NodeInfo => ({
    endpoint: `nexo${String(n)}:9000`,
    state: 'online',
    version: '0.1.0-dev',
    commit: 'abc1234',
    local: false,
    uptimeSeconds: uptime - n * 60,
    disks: [clusterDisk(n, 1, 'online'), clusterDisk(n, 2, 'online')],
    healQueue: { pending: 0, healed: 0, retried: 0, dropped: 0 },
    clockSkew: { offsetMs: 3, rttMs: 1, exceeded: false },
    ...extra,
  });
  const healing = clusterDisk(3, 2, 'healing');
  healing.healingProgress = {
    started: '2026-10-07T08:00:00Z',
    lastUpdate: '2026-10-07T08:05:00Z',
    bucket: 'photos',
    object: '2026/beach.jpg',
    healedBuckets: 1,
    objectsHealed: 128,
    objectsFailed: 0,
    bytesDone: 3 * GiB,
    bytesFailed: 0,
  };
  return sampleServerInfo({
    mode: 'distributed',
    pools: [
      {
        index: 0,
        sets: 1,
        drivesPerSet: 8,
        parity: 4,
        readQuorum: 4,
        writeQuorum: 5,
        erasureSets: [
          { index: 0, drives: 8, online: 6, offline: 2, healing: 1, state: 'degraded' },
        ],
      },
    ],
    nodes: [
      node(1, {
        local: true,
        healQueue: { pending: 2, healed: 14, retried: 3, dropped: 0 },
        clockSkew: undefined,
      }),
      node(2, {
        state: 'offline',
        version: '',
        commit: '',
        uptimeSeconds: 0,
        disks: [clusterDisk(2, 1, 'offline'), clusterDisk(2, 2, 'offline')],
        healQueue: undefined,
        clockSkew: undefined,
      }),
      node(3, { disks: [clusterDisk(3, 1, 'online'), healing] }),
      node(4, { clockSkew: { offsetMs: 2600, rttMs: 2, exceeded: true } }),
    ],
    ...overrides,
  });
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
