import type { PoolsService } from '../interfaces';
import type { PoolJob, Pools, PoolStatus, ServerInfo } from '../types';
import { clone, problem, type MockContext } from './support';

export interface MockPoolsOptions {
  /** The pools as GET /pools reports them; by default one per pool of the server information. */
  pools?: Pools;
  /**
   * Reads (list) of a started decommission or rebalance before it
   * completes (default 1: the first poll sees it running).
   */
  poolPolls?: number;
}

/** The answer of a single-drive deployment (501). */
const notImplemented = () =>
  problem(501, 'not_implemented', 'pool management needs an erasure-coded deployment');

function newJob(now: string): PoolJob {
  return {
    state: 'running',
    startedAt: now,
    updatedAt: now,
    pass: 1,
    objects: 0,
    versions: 0,
    bytes: 0,
    failed: 0,
    pendingUploads: 0,
  };
}

/** The pools of the server information, with the usage of their drives. */
function poolsOf(info: ServerInfo): Pools {
  return {
    pools: info.pools.map((p) => {
      const disks = info.nodes.flatMap((n) => n.disks.filter((d) => d.poolIdx === p.index));
      const total = disks.reduce((sum, d) => sum + d.total, 0);
      const used = disks.reduce((sum, d) => sum + d.used, 0);
      return {
        index: p.index + 1,
        id: `pool${String(p.index + 1)}`,
        total,
        free: total - used,
        draining: false,
      };
    }),
  };
}

/**
 * PoolsService in memory. A decommission drains the pool: after
 * `poolPolls` reads it completes, its used space moved to the other
 * pools. A rebalance completes the same way.
 */
export function createMockPoolsService(
  ctx: MockContext,
  info: () => ServerInfo,
  options: MockPoolsOptions = {},
): PoolsService {
  let state: Pools | undefined = options.pools ? clone(options.pools) : undefined;
  const pollsBeforeDone = options.poolPolls ?? 1;
  const polls = new Map<PoolJob, number>();

  const current = (): Pools => {
    if (info().mode === 'snsd') {
      throw notImplemented();
    }
    state ??= poolsOf(info());
    return state;
  };
  const find = (pool: number): PoolStatus => {
    const p = current().pools.find((s) => s.index === pool);
    if (p === undefined) {
      throw problem(400, 'invalid_argument', `there is no pool ${String(pool)}`);
    }
    return p;
  };
  const advance = (job: PoolJob | undefined, done: () => void) => {
    if (job?.state !== 'running') {
      return;
    }
    const seen = (polls.get(job) ?? 0) + 1;
    polls.set(job, seen);
    if (seen > pollsBeforeDone) {
      done();
      job.state = 'complete';
      job.finishedAt = ctx.nowIso();
      job.updatedAt = job.finishedAt;
    }
  };

  return {
    async list() {
      await ctx.delay();
      const pools = current();
      for (const p of pools.pools) {
        const job = p.decommission;
        advance(job, () => {
          if (job === undefined) return;
          const used = p.total - p.free;
          const others = pools.pools.filter((o) => !o.draining);
          for (const o of others) {
            o.free = Math.max(0, o.free - Math.ceil(used / others.length));
          }
          job.objects = used > 0 ? Math.max(1, Math.round(used / (1024 * 1024))) : 0;
          job.versions = job.objects;
          job.bytes = used;
          p.free = p.total;
        });
      }
      advance(pools.rebalance, () => undefined);
      return clone(pools);
    },
    async decommission(pool) {
      await ctx.delay();
      const p = find(pool);
      if (pool === 1) {
        throw problem(
          400,
          'invalid_argument',
          'the first pool holds the system documents and cannot be decommissioned',
        );
      }
      if (current().pools.filter((o) => !o.draining && o !== p).length === 0) {
        throw problem(400, 'invalid_argument', 'no other pool would take the objects');
      }
      if (p.decommission?.state === 'running') {
        return;
      }
      p.draining = true;
      p.decommission = newJob(ctx.nowIso());
    },
    async cancelDecommission(pool) {
      await ctx.delay();
      const p = find(pool);
      if (p.decommission?.state !== 'running') {
        throw problem(400, 'invalid_argument', `pool ${String(pool)} is not being decommissioned`);
      }
      p.draining = false;
      p.decommission.state = 'canceled';
      p.decommission.finishedAt = ctx.nowIso();
    },
    async startRebalance() {
      await ctx.delay();
      const pools = current();
      if (pools.rebalance?.state === 'running') {
        return;
      }
      pools.rebalance = newJob(ctx.nowIso());
    },
    async stopRebalance() {
      await ctx.delay();
      const job = current().rebalance;
      if (job?.state !== 'running') {
        throw problem(400, 'invalid_argument', 'no rebalance is running');
      }
      job.state = 'canceled';
      job.finishedAt = ctx.nowIso();
    },
  };
}
