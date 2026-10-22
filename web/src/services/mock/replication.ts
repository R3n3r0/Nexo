import type { ReplicationService } from '../interfaces';
import type {
  ReplicationResync,
  ReplicationRule,
  ReplicationStatus,
  ReplicationTarget,
} from '../types';
import type { MockStorage } from './storage';
import { clone, problem } from './support';

interface BucketReplication {
  targets: ReplicationTarget[];
  rules: ReplicationRule[];
  resync?: ReplicationResync;
}

/** A short stable hex digest of a string (the ARN id of a target). */
function digest(text: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    h = Math.imul(h ^ text.charCodeAt(i), 0x01000193) >>> 0;
  }
  return (h.toString(16).padStart(8, '0') + text.length.toString(16).padStart(4, '0')).slice(0, 12);
}

/**
 * ReplicationService in memory: the targets get an ARN from their endpoint
 * and bucket like on the server, the rules must name them and need an
 * enabled versioning, and a resync completes on the next status read.
 */
export function createMockReplicationService(storage: MockStorage): ReplicationService {
  const { ctx } = storage;
  const state = new Map<string, BucketReplication>();
  const of = (bucket: string): BucketReplication => {
    storage.bucket(bucket);
    let s = state.get(bucket);
    if (s === undefined) {
      s = { targets: [], rules: [] };
      state.set(bucket, s);
    }
    return s;
  };
  const withoutSecret = (t: ReplicationTarget): ReplicationTarget => {
    const out = clone(t);
    delete out.secretKey;
    return out;
  };

  return {
    async get(bucket) {
      await ctx.delay();
      return { rules: clone(of(bucket).rules) };
    },
    async set(bucket, config) {
      await ctx.delay();
      const s = of(bucket);
      if (config.rules.length > 0 && storage.bucket(bucket).bucket.versioning !== 'enabled') {
        throw problem(
          400,
          'invalid_argument',
          'replication needs the versioning of the bucket enabled',
        );
      }
      const ids = new Set<string>();
      const priorities = new Set<number>();
      for (const rule of config.rules) {
        if (rule.id === '' || ids.has(rule.id)) {
          throw problem(400, 'invalid_argument', `duplicate or empty rule ID "${rule.id}"`);
        }
        ids.add(rule.id);
        if (priorities.has(rule.priority)) {
          throw problem(
            400,
            'invalid_argument',
            `rule "${rule.id}": priority ${String(rule.priority)} used twice`,
          );
        }
        priorities.add(rule.priority);
        if (!s.targets.some((t) => t.arn === rule.target)) {
          throw problem(
            400,
            'invalid_argument',
            `rule "${rule.id}": "${rule.target}" is not a replication target of the bucket`,
          );
        }
      }
      s.rules = clone(config.rules);
      return { rules: clone(s.rules) };
    },
    async remove(bucket) {
      await ctx.delay();
      of(bucket).rules = [];
    },
    async targets(bucket) {
      await ctx.delay();
      return of(bucket).targets.map(withoutSecret);
    },
    async setTarget(bucket, target) {
      await ctx.delay();
      const s = of(bucket);
      if (!/^https?:\/\/[^/]+$/.test(target.endpoint.replace(/\/+$/, '')) || target.bucket === '') {
        throw problem(400, 'invalid_argument', 'a target needs an http(s) endpoint and a bucket');
      }
      if (target.accessKey === '') {
        throw problem(400, 'invalid_argument', 'a target needs an access key');
      }
      const endpoint = target.endpoint.replace(/\/+$/, '');
      const arn = `arn:nexo:replication::${digest(`${endpoint}\u0000${target.bucket}`)}:${target.bucket}`;
      const now = ctx.nowIso();
      const index = s.targets.findIndex((t) => t.arn === arn);
      const old = index >= 0 ? s.targets[index] : undefined;
      if (old === undefined && !target.secretKey) {
        throw problem(400, 'invalid_argument', 'a new target needs a secret key');
      }
      const stored: ReplicationTarget = {
        ...clone(target),
        arn,
        endpoint,
        secretKey: target.secretKey || old?.secretKey,
        created: old?.created ?? now,
        updated: now,
      };
      if (old === undefined) s.targets.push(stored);
      else s.targets[index] = stored;
      return withoutSecret(stored);
    },
    async removeTarget(bucket, arn) {
      await ctx.delay();
      const s = of(bucket);
      if (s.rules.some((r) => r.target === arn)) {
        throw problem(409, 'conflict', `the replication rules use the target ${arn}`);
      }
      if (!s.targets.some((t) => t.arn === arn)) {
        throw problem(404, 'not_found', `no replication target ${arn}`);
      }
      s.targets = s.targets.filter((t) => t.arn !== arn);
    },
    async status(bucket) {
      await ctx.delay();
      const s = of(bucket);
      const out: ReplicationStatus = {
        queued: 0,
        targets: s.targets.map((t) => ({
          arn: t.arn ?? '',
          online: true,
          queued: 0,
          replicated: 0,
          bytes: 0,
          deletes: 0,
          failed: 0,
        })),
        ...(s.resync ? { resync: clone(s.resync) } : {}),
      };
      if (s.resync?.state === 'running') {
        // The demo resync completes on the next read.
        s.resync = {
          ...s.resync,
          state: 'complete',
          finishedAt: ctx.nowIso(),
          updatedAt: ctx.nowIso(),
        };
      }
      return out;
    },
    async resync(bucket, target) {
      await ctx.delay();
      const s = of(bucket);
      if (!s.rules.some((r) => r.target === target)) {
        throw problem(400, 'invalid_argument', `no replication rule of the bucket uses ${target}`);
      }
      if (s.resync?.state === 'running') {
        throw problem(409, 'conflict', 'a resync of the bucket is running');
      }
      const now = ctx.nowIso();
      const keys = storage.bucket(bucket).keys.size;
      s.resync = {
        id: storage.nextId('resync-'),
        target,
        state: 'running',
        startedAt: now,
        updatedAt: now,
        scanned: keys,
        queued: keys,
      };
      return clone(s.resync);
    },
  };
}
