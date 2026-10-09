import { describe, expect, it } from 'vitest';

import { sampleClusterInfo, sampleServerInfo } from '@/services/mock';

import {
  clusterHealth,
  driveState,
  formatSkew,
  healOptions,
  healTarget,
  nodesOfPool,
} from './cluster';

describe('cluster helpers', () => {
  it('summarizes a degraded cluster', () => {
    const health = clusterHealth(sampleClusterInfo());
    expect(health).toEqual({
      nodes: 4,
      nodesOnline: 3,
      offlineNodes: ['nexo2:9000'],
      drives: 8,
      drivesOnline: 6,
      drivesOffline: 2,
      drivesHealing: 1,
      sets: 1,
      setsOk: 0,
      state: 'degraded',
      maxSkewMs: 2600,
      skewedNodes: ['nexo4:9000'],
      healPending: 2,
      healHealed: 14,
    });
  });

  it('reports a single drive as healthy without clocks', () => {
    const health = clusterHealth(sampleServerInfo());
    expect(health.state).toBe('ok');
    expect(health.maxSkewMs).toBeUndefined();
    expect(health.offlineNodes).toEqual([]);
    expect(health.setsOk).toBe(1);
  });

  it('takes the worst set state', () => {
    const info = sampleClusterInfo();
    const pool = info.pools[0];
    if (!pool) throw new Error('fixture');
    pool.erasureSets.push({
      index: 1,
      drives: 8,
      online: 3,
      offline: 5,
      healing: 0,
      state: 'unavailable',
    });
    expect(clusterHealth(info).state).toBe('unavailable');
  });

  it('groups the drives of a pool by node', () => {
    const info = sampleClusterInfo();
    const nodes = nodesOfPool(info, 0);
    expect(nodes.map((n) => n.node.endpoint)).toEqual([
      'nexo1:9000',
      'nexo2:9000',
      'nexo3:9000',
      'nexo4:9000',
    ]);
    expect(nodes.every((n) => n.disks.length === 2)).toBe(true);
    expect(nodesOfPool(info, 1)).toEqual([]);
  });

  it('shows a healing drive as healing', () => {
    const disk = sampleClusterInfo().nodes[2]?.disks[1];
    if (!disk) throw new Error('fixture');
    expect(driveState(disk)).toBe('healing');
    expect(driveState({ ...disk, healing: false, state: 'online' })).toBe('online');
  });

  it.each([
    [0, '0 ms'],
    [3, '+3 ms'],
    [-999, '−999 ms'],
    [2600, '+2.60 s'],
    [-12_345, '−12.35 s'],
  ])('formats a clock offset of %d ms', (ms, want) => {
    expect(formatSkew(ms, 'en')).toBe(want);
  });

  it('describes the target and the options of a request', () => {
    expect(healTarget({})).toBeUndefined();
    expect(healTarget({ bucket: 'photos' })).toBe('photos');
    expect(healTarget({ bucket: 'photos', prefix: '2026/' })).toBe('photos/2026/');
    expect(healOptions({ recursive: true, dryRun: false, deep: true, remove: true })).toEqual([
      'recursive',
      'deep',
      'remove',
    ]);
  });
});
