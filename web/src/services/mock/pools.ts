import type { PoolsService } from '../interfaces';
import type { PlannedPool, PoolJob, PoolPlan, Pools, PoolStatus, ServerInfo } from '../types';
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

/** Writes distinct values with a numeric suffix as one ellipsis ("nexo{1...4}"). */
function compress(values: string[]): string {
  const distinct = [...new Set(values)];
  if (distinct.length === 1) return distinct[0] ?? '';
  const parts = distinct.map((v) => /^(.*?)(\d+)$/.exec(v));
  const prefix = parts[0]?.[1];
  const nums = parts.map((m) => Number(m?.[2]));
  if (parts.some((m) => m === null || m[1] !== prefix) || nums.some(Number.isNaN)) {
    return distinct.join(',');
  }
  return `${prefix ?? ''}{${String(Math.min(...nums))}...${String(Math.max(...nums))}}`;
}

/** The command-line argument of a pool, from the endpoints of its drives. */
function poolArg(info: ServerInfo, pool: number): string {
  const disks = info.nodes.flatMap((n) => n.disks.filter((d) => d.poolIdx === pool));
  const paths = compress(disks.map((d) => d.path ?? '/data'));
  if (info.mode !== 'distributed') return paths;
  const hosts = disks.map((d) => /^https?:\/\/([^/:]+)/.exec(d.endpoint)?.[1] ?? 'nexo');
  const port = /:(\d+)/.exec(disks[0]?.endpoint ?? '')?.[1] ?? '9000';
  return `http://${compress(hosts)}:${port}${paths}`;
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
        id: poolArg(info, p.index),
        total,
        free: total - used,
        draining: false,
      };
    }),
  };
}

/** Expands the ellipses of an argument ("/d{1...4}") into its endpoints. */
function expand(arg: string): string[] {
  const m = /\{(\d+)\.\.\.(\d+)\}/.exec(arg);
  if (m === null) return [arg];
  const [whole, from = '', to = ''] = m;
  const lo = Number(from);
  const hi = Number(to);
  if (hi < lo || hi - lo > 1024) {
    throw problem(400, 'invalid_argument', `topology: invalid ellipsis pattern in ${arg}`);
  }
  const out: string[] = [];
  for (let i = lo; i <= hi; i++) {
    const n = String(i).padStart(from.length, '0');
    out.push(...expand(arg.slice(0, m.index) + n + arg.slice(m.index + whole.length)));
  }
  return out;
}

/** The node of an endpoint: host:port, "local" for a path. */
const nodeOf = (endpoint: string) => /^https?:\/\/([^/]+)/.exec(endpoint)?.[1] ?? 'local';

/** The largest erasure set of 2 to 16 drives that splits the pool evenly over its nodes. */
function setSize(drives: number, nodes: number): number {
  for (let size = 16; size >= 2; size--) {
    if (drives % size === 0 && (size % nodes === 0 || nodes % size === 0)) return size;
  }
  return 0;
}

const defaultParity = (size: number) =>
  size >= 6 ? Math.min(4, Math.floor(size / 2)) : size >= 4 ? 2 : 1;

/** A pool of the plan, as the topology parses its argument. */
function plannedPool(arg: string, index: number, current: number): PlannedPool {
  if (!arg.includes('...') || /\s/.test(arg)) {
    throw problem(400, 'invalid_argument', `${arg}: one argument with ellipses per pool`);
  }
  const endpoints = expand(arg);
  if (endpoints.some((e) => !e.startsWith('/') && !/^https?:\/\/[^/]+\/./.test(e))) {
    throw problem(400, 'invalid_argument', `topology: drive path must be absolute (${arg})`);
  }
  const nodes = [...new Set(endpoints.map(nodeOf))];
  const size = setSize(endpoints.length, nodes.length);
  if (size === 0) {
    throw problem(
      400,
      'invalid_argument',
      `pool ${String(index)} (${arg}): topology: cannot split the drives into erasure sets of 2 to 16 drives distributed uniformly across nodes`,
    );
  }
  return {
    index,
    ...(current > 0 ? { current } : {}),
    arg,
    nodes,
    drives: endpoints.length,
    sets: endpoints.length / size,
    drivesPerSet: size,
    parity: defaultParity(size),
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
    async plan(input) {
      await ctx.delay();
      const pools = current();
      const add = (input.add ?? []).map((a) => a.trim());
      const remove = input.remove ?? [];
      if (add.length === 0 && remove.length === 0) {
        throw problem(400, 'invalid_argument', 'add or remove at least one pool');
      }
      for (const r of remove) {
        if (find(r).decommission?.state !== 'complete') {
          throw problem(
            400,
            'invalid_argument',
            `pool ${String(r)} is not decommissioned: decommission it and wait for complete first`,
          );
        }
      }
      const kept = pools.pools.filter((p) => !remove.includes(p.index));
      const args = [...kept.map((p) => p.id), ...add];
      const planned = args.map((arg, i) => plannedPool(arg, i + 1, kept[i]?.index ?? 0));
      const all = planned.flatMap((p) => expand(p.arg));
      const dup = all.find((e, i) => all.indexOf(e) !== i);
      if (dup !== undefined) {
        throw problem(400, 'invalid_argument', `topology: duplicate endpoint: ${dup}`);
      }
      if (new Set(all.map((e) => e.startsWith('/'))).size > 1) {
        throw problem(
          400,
          'invalid_argument',
          'topology: cannot mix local paths and URL endpoints',
        );
      }
      const known = new Set(info().nodes.map((n) => n.endpoint));
      const nodes = [...new Set(planned.flatMap((p) => p.nodes))];
      const plan: PoolPlan = {
        mode: info().mode,
        args,
        pools: planned,
        removed: pools.pools.filter((p) => remove.includes(p.index)).map((p) => p.id),
        nodes,
        newNodes: nodes.filter((n) => n !== 'local' && !known.has(n)),
      };
      return plan;
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
