import type {
  DiskInfo,
  ErasureSetState,
  HealJob,
  HealRequest,
  NodeInfo,
  ResourceState,
  ServerInfo,
} from '@/services';

/** TanStack Query keys of the healing jobs (the server information is serverInfoQueryKey). */
export const healKeys = {
  jobs: ['heal', 'jobs'] as const,
  job: (id: string) => ['heal', 'jobs', id] as const,
};

/** Polling of the cluster pages: fast while something changes, slow otherwise. */
export const clusterRefreshMs = 5_000;
export const healRunningRefreshMs = 1_000;
export const healIdleRefreshMs = 10_000;

/** The state a drive is shown with: healing wins over online. */
export function driveState(disk: DiskInfo): ResourceState {
  return disk.healing ? 'healing' : disk.state;
}

/** Badge colours of the node and drive states. */
export const resourceColor: Record<ResourceState, string> = {
  online: 'green',
  offline: 'red',
  healing: 'yellow',
  unknown: 'gray',
};

/** Badge colours of the erasure set states. */
export const setStateColor: Record<ErasureSetState, string> = {
  ok: 'green',
  degraded: 'yellow',
  readonly: 'orange',
  unavailable: 'red',
};

/** Worst first: the overall state of the deployment is the worst set state. */
const setSeverity: Record<ErasureSetState, number> = {
  ok: 0,
  degraded: 1,
  readonly: 2,
  unavailable: 3,
};

/** A node with only its drives of one pool. */
export interface PoolNode {
  node: NodeInfo;
  disks: DiskInfo[];
}

/** The nodes holding drives of a pool, in the order of the server, with those drives. */
export function nodesOfPool(info: ServerInfo, poolIdx: number): PoolNode[] {
  const out: PoolNode[] = [];
  for (const node of info.nodes) {
    const disks = node.disks
      .filter((d) => d.poolIdx === poolIdx)
      .sort((a, b) => a.setIdx - b.setIdx || a.diskIdx - b.diskIdx);
    if (disks.length > 0) {
      out.push({ node, disks });
    }
  }
  return out;
}

/** What the cluster page summarizes at the top. */
export interface ClusterHealth {
  nodes: number;
  nodesOnline: number;
  /** Endpoints of the nodes that do not answer. */
  offlineNodes: string[];
  drives: number;
  drivesOnline: number;
  drivesOffline: number;
  drivesHealing: number;
  sets: number;
  setsOk: number;
  /** The worst state of an erasure set ('ok' without sets). */
  state: ErasureSetState;
  /** Largest absolute clock offset among the measured nodes, in ms. */
  maxSkewMs: number | undefined;
  /** Endpoints of the nodes whose clock is off by more than the threshold. */
  skewedNodes: string[];
  /** Background healing (MRF) of every node: objects waiting and repaired. */
  healPending: number;
  healHealed: number;
}

/** Summarizes nodes, drives, sets, clocks and background healing. */
export function clusterHealth(info: ServerInfo): ClusterHealth {
  const health: ClusterHealth = {
    nodes: info.nodes.length,
    nodesOnline: 0,
    offlineNodes: [],
    drives: 0,
    drivesOnline: 0,
    drivesOffline: 0,
    drivesHealing: 0,
    sets: 0,
    setsOk: 0,
    state: 'ok',
    maxSkewMs: undefined,
    skewedNodes: [],
    healPending: 0,
    healHealed: 0,
  };
  for (const node of info.nodes) {
    if (node.state === 'offline') {
      health.offlineNodes.push(node.endpoint);
    } else {
      health.nodesOnline += 1;
    }
    for (const disk of node.disks) {
      health.drives += 1;
      const state = driveState(disk);
      if (state === 'offline') health.drivesOffline += 1;
      else health.drivesOnline += 1;
      if (state === 'healing') health.drivesHealing += 1;
    }
    if (node.clockSkew) {
      const abs = Math.abs(node.clockSkew.offsetMs);
      health.maxSkewMs = Math.max(health.maxSkewMs ?? 0, abs);
      if (node.clockSkew.exceeded) health.skewedNodes.push(node.endpoint);
    }
    if (node.healQueue) {
      health.healPending += node.healQueue.pending;
      health.healHealed += node.healQueue.healed;
    }
  }
  for (const pool of info.pools) {
    for (const set of pool.erasureSets) {
      health.sets += 1;
      if (set.state === 'ok') health.setsOk += 1;
      if (setSeverity[set.state] > setSeverity[health.state]) health.state = set.state;
    }
  }
  return health;
}

/** A clock offset for humans: "+2.60 s", "−3 ms", "0 ms". */
export function formatSkew(offsetMs: number, locale?: string): string {
  const sign = offsetMs > 0 ? '+' : offsetMs < 0 ? '−' : '';
  const abs = Math.abs(offsetMs);
  if (abs < 1000) {
    return `${sign}${new Intl.NumberFormat(locale).format(abs)} ms`;
  }
  return `${sign}${new Intl.NumberFormat(locale, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(abs / 1000)} s`;
}

/** Ratio of the used capacity of a drive (0 when unknown). */
export function usedRatio(disk: DiskInfo): number {
  return disk.total > 0 ? disk.used / disk.total : 0;
}

/** True when a job may still change (polled faster). */
export function isRunning(job: HealJob): boolean {
  return !job.finished;
}

/** The target of a healing request: "photos/2026/", "photos", or undefined for everything. */
export function healTarget(request: HealRequest): string | undefined {
  if (request.bucket === undefined || request.bucket === '') {
    return undefined;
  }
  const prefix = request.prefix ?? '';
  return prefix === '' ? request.bucket : `${request.bucket}/${prefix}`;
}

/** The options set on a request, in a stable order (for badges). */
export function healOptions(request: HealRequest): ('recursive' | 'dryRun' | 'deep' | 'remove')[] {
  const out: ('recursive' | 'dryRun' | 'deep' | 'remove')[] = [];
  if (request.recursive === true) out.push('recursive');
  if (request.dryRun === true) out.push('dryRun');
  if (request.deep === true) out.push('deep');
  if (request.remove === true) out.push('remove');
  return out;
}

/** Badge colours of the job states. */
export const jobStateColor: Record<HealJob['state'], string> = {
  running: 'blue',
  done: 'green',
  failed: 'red',
  canceled: 'gray',
};
