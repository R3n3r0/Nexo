import type { HealService } from '../interfaces';
import type {
  HealItem,
  HealItemDrive,
  HealJob,
  HealJobDrive,
  HealRequest,
  ServerInfo,
} from '../types';
import { clone, problem, type MockContext } from './support';

export interface MockHealOptions {
  /** Jobs already listed (newest first), e.g. for the demo. */
  healJobs?: HealJob[];
  /**
   * Reads of a started job (get or list) before it finishes; 0 finishes
   * it at once (default 1: the first poll sees it running).
   */
  healPolls?: number;
}

/** The answer of a single-drive deployment (POST/GET /heal: 501). */
const notImplemented = () =>
  problem(501, 'not_implemented', 'healing needs an erasure-coded deployment (several drives)');

/** A job as the server reports it right after POST /heal. */
function newJob(id: string, request: HealRequest, drives: HealJobDrive[], now: string): HealJob {
  return {
    id,
    state: 'running',
    request,
    finished: false,
    node: 'nexo1:9000',
    startedAt: now,
    updatedAt: now,
    current: request.bucket ?? '',
    scanned: 0,
    healed: 0,
    failed: 0,
    dangling: 0,
    removed: 0,
    bytes: 0,
    drives,
    items: [],
  };
}

/**
 * HealService in memory. A job runs over the drives of the server
 * information: it finds one object missing on every healing drive and
 * reports the offline drives, repairs the object (unless dryRun) and
 * finishes after `healPolls` reads.
 */
export function createMockHealService(
  ctx: MockContext,
  info: () => ServerInfo,
  options: MockHealOptions = {},
): HealService {
  const jobs: HealJob[] = clone(options.healJobs ?? []);
  const polls = new Map<string, number>();
  const pollsBeforeDone = options.healPolls ?? 1;
  let sequence = jobs.length;

  const finish = (job: HealJob) => {
    const now = ctx.nowIso();
    const dryRun = job.request.dryRun === true;
    const drives: HealItemDrive[] = job.drives.map((d) => {
      const healing = info().nodes.some((n) =>
        n.disks.some((disk) => disk.endpoint === d.endpoint && disk.healing),
      );
      if (d.state === 'offline') {
        d.offline += 1;
        return { ...pick(d), before: 'offline', after: 'offline' };
      }
      if (healing) {
        d.missing += 1;
        d.healed += dryRun ? 0 : 1;
        return { ...pick(d), before: 'missing', after: dryRun ? 'missing' : 'ok' };
      }
      return { ...pick(d), before: 'ok', after: 'ok' };
    });
    const needed = drives.some((d) => d.before !== 'ok');
    const objects = job.request.recursive === true ? 1 : 0;
    job.scanned = 1 + objects + (job.request.bucket === undefined ? 1 : 0);
    if (needed && objects > 0) {
      const item: HealItem = {
        kind: 'object',
        bucket: job.request.bucket ?? '',
        object: `${job.request.prefix ?? ''}report.pdf`,
        healed: drives.filter((d) => d.before === 'missing' && d.after === 'ok').length,
        drives,
      };
      job.items = [item];
      job.healed = 1;
      job.bytes = 1024 * 1024;
    }
    job.state = 'done';
    job.finished = true;
    job.current = '';
    job.updatedAt = now;
    job.finishedAt = now;
  };

  const read = (job: HealJob): HealJob => {
    if (!job.finished) {
      const seen = (polls.get(job.id) ?? 0) + 1;
      polls.set(job.id, seen);
      if (seen > pollsBeforeDone) {
        finish(job);
      }
    }
    return clone(job);
  };

  const ensureErasure = () => {
    if (info().mode === 'snsd') {
      throw notImplemented();
    }
  };

  return {
    async start(request) {
      await ctx.delay();
      ensureErasure();
      if (request.prefix !== undefined && request.prefix !== '' && request.bucket === undefined) {
        throw problem(400, 'invalid_argument', 'a prefix needs a bucket');
      }
      const same = jobs.find(
        (j) => !j.finished && JSON.stringify(j.request) === JSON.stringify(request),
      );
      if (same) {
        return clone(same);
      }
      sequence += 1;
      const drives: HealJobDrive[] = info().nodes.flatMap((n) =>
        n.disks.map((d) => ({
          endpoint: d.endpoint,
          pool: d.poolIdx,
          set: d.setIdx,
          disk: d.diskIdx,
          state: d.state === 'offline' ? ('offline' as const) : ('online' as const),
          missing: 0,
          corrupt: 0,
          offline: 0,
          healed: 0,
        })),
      );
      const job = newJob(
        `heal-${String(sequence).padStart(4, '0')}`,
        request,
        drives,
        ctx.nowIso(),
      );
      jobs.unshift(job);
      if (pollsBeforeDone === 0) {
        finish(job);
      }
      return clone(job);
    },
    async list() {
      await ctx.delay();
      ensureErasure();
      return jobs.map(read);
    },
    async get(id) {
      await ctx.delay();
      ensureErasure();
      const job = jobs.find((j) => j.id === id);
      if (job === undefined) {
        throw problem(404, 'not_found', `healing job ${id} not found`);
      }
      return read(job);
    },
  };
}

/** The drive coordinates of a job drive, for its items. */
function pick(d: HealJobDrive): Omit<HealItemDrive, 'before' | 'after'> {
  return { endpoint: d.endpoint, pool: d.pool, set: d.set, disk: d.disk };
}
