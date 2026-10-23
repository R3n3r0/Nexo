import type { MaintenanceService } from '../interfaces';
import type { RestartMode, ServerInfo, UpdateJob } from '../types';
import { clone, problem, type MockContext } from './support';

const sha256Pattern = /^[0-9a-f]{64}$/i;

/**
 * MaintenanceService in memory: a job moves one node to done at each read
 * (job()), the node that answered (the first) last; an update reports the
 * version of the new binary. stop() is accepted and changes nothing.
 */
export function createMockMaintenanceService(
  ctx: MockContext,
  info: () => ServerInfo,
): MaintenanceService {
  let current: UpdateJob | null = null;
  let seq = 0;

  const start = (kind: UpdateJob['kind'], mode: RestartMode, extra: Partial<UpdateJob>) => {
    if (current?.state === 'running') {
      throw problem(409, 'conflict', `a job is running (${current.id})`);
    }
    const nodes = info().nodes.map((n) => n.endpoint);
    seq += 1;
    current = {
      id: `job-${String(seq)}`,
      kind,
      mode,
      state: 'running',
      node: nodes[0] ?? 'local',
      started: ctx.nowIso(),
      nodes: nodes.map((node) => ({ node, phase: kind === 'update' ? 'staged' : 'pending' })),
      ...extra,
    };
    return clone(current);
  };

  return {
    async job() {
      await ctx.delay();
      const job = current;
      if (job === null) return null;
      if (job.state === 'running') {
        // The node that answered restarts last.
        const next = [...job.nodes.slice(1), job.nodes[0]].find((n) => n && n.phase !== 'done');
        if (next === undefined) {
          job.state = 'completed';
          job.finished = ctx.nowIso();
        } else {
          next.phase = 'done';
          if (job.version !== undefined) next.version = job.version;
        }
      }
      return clone(job);
    },
    async update(input) {
      await ctx.delay();
      if (!/^https?:\/\/\S+$/.test(input.url)) {
        throw problem(400, 'invalid_argument', 'the binary URL must be http(s)');
      }
      if (!sha256Pattern.test(input.sha256)) {
        throw problem(400, 'invalid_argument', 'the SHA-256 must be 64 hex digits');
      }
      return start('update', input.mode ?? 'rolling', {
        url: input.url,
        sha256: input.sha256.toLowerCase(),
        version: 'v1.1.0',
      });
    },
    async restart(mode) {
      await ctx.delay();
      return start('restart', mode, {});
    },
    async stop() {
      await ctx.delay();
    },
  };
}
