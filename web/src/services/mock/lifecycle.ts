import { validateRule } from '@/lib/lifecycle';

import type { LifecycleService } from '../interfaces';
import type { LifecycleRule } from '../types';
import type { RuleProblem } from '../errors';
import { lockOf, type MockStorage } from './storage';
import { clone, invalidConfiguration } from './support';

/** Messages of the problems of a rule, as the server words them. */
const messages: Record<string, { code: string; message: string }> = {
  idTooLong: {
    code: 'InvalidArgument',
    message: 'ID length should not exceed allowed limit of 255',
  },
  idDuplicate: { code: 'InvalidArgument', message: 'Rule ID must be unique.' },
  prefixTooLong: {
    code: 'InvalidArgument',
    message: 'The prefix cannot be longer than 1024 characters',
  },
  tagKey: {
    code: 'InvalidArgument',
    message: 'The TagKey or TagValue you have provided is invalid',
  },
  tagTooLong: {
    code: 'InvalidArgument',
    message: 'The TagKey or TagValue you have provided is invalid',
  },
  tagDuplicate: { code: 'InvalidArgument', message: 'Duplicate Tag Keys are not allowed' },
  sizeNegative: { code: 'InvalidArgument', message: 'Object size filters must be non-negative' },
  sizeRange: {
    code: 'InvalidArgument',
    message: 'ObjectSizeGreaterThan must be less than ObjectSizeLessThan',
  },
  positive: { code: 'InvalidArgument', message: "'Days' must be a positive integer" },
  date: { code: 'InvalidArgument', message: "'Date' must be at midnight GMT" },
  expirationOne: {
    code: 'InvalidRequest',
    message: 'Expiration must have exactly one of Days, Date or ExpiredObjectDeleteMarker',
  },
  newerRange: {
    code: 'InvalidArgument',
    message: "'NewerNoncurrentVersions' must be between 1 and 100",
  },
  noAction: {
    code: 'InvalidRequest',
    message: 'At least one action needs to be specified in a rule',
  },
  abortWithFilter: {
    code: 'InvalidRequest',
    message: 'AbortIncompleteMultipartUpload cannot be specified with Tags or object size filters',
  },
  deleteMarkerWithTags: {
    code: 'InvalidRequest',
    message: 'ExpiredObjectDeleteMarker cannot be specified with Tags',
  },
};

/** Validates the rules one by one, like the Admin API (ruleErrors, 1-based). */
export function lifecycleProblems(rules: readonly LifecycleRule[]): RuleProblem[] {
  const problems: RuleProblem[] = [];
  rules.forEach((rule, i) => {
    const others = rules.filter((_, j) => j < i).map((r) => r.id ?? '');
    let issues = validateRule(rule, others);
    if ((rule.transitions?.length ?? 0) > 0 || (rule.noncurrentTransitions?.length ?? 0) > 0) {
      problems.push({
        rule: i + 1,
        ...(rule.id !== undefined ? { id: rule.id } : {}),
        code: 'NotImplemented',
        message: 'Transitions need a remote tier, which is not configured.',
      });
      issues = issues.filter((issue) => issue !== 'noAction');
    }
    const first = issues[0];
    if (first !== undefined) {
      const m = messages[first] ?? { code: 'InvalidArgument', message: first };
      problems.push({ rule: i + 1, ...(rule.id !== undefined ? { id: rule.id } : {}), ...m });
    }
  });
  return problems;
}

const day = 24 * 3600 * 1000;

/** Lifecycle rules kept in the mock buckets. */
export function createMockLifecycleService(storage: MockStorage): LifecycleService {
  const { ctx } = storage;
  let generated = 0;

  return {
    async get(bucket) {
      await ctx.delay();
      return { rules: clone(storage.bucket(bucket).lifecycle ?? []) };
    },
    async set(bucket, config) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const problems = lifecycleProblems(config.rules);
      if (problems.length > 0) {
        throw invalidConfiguration(problems);
      }
      const rules = config.rules.map((rule) => {
        const out = clone(rule);
        if (out.id === undefined || out.id === '') {
          generated += 1;
          out.id = `nexo-rule-${String(generated)}`;
        }
        out.enabled = out.enabled !== false;
        return out;
      });
      if (rules.length === 0) {
        delete state.lifecycle;
      } else {
        state.lifecycle = rules;
      }
      return { rules: clone(rules) };
    },
    async remove(bucket) {
      await ctx.delay();
      delete storage.bucket(bucket).lifecycle;
    },
    async apply(bucket) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const rules = (state.lifecycle ?? []).filter((r) => r.enabled !== false);
      const result = {
        buckets: state.lifecycle ? 1 : 0,
        versions: 0,
        deleted: 0,
        deleteMarkers: 0,
        uploadsAborted: 0,
        locked: 0,
      };
      const now = ctx.now().getTime();
      for (const [key, versions] of [...state.keys]) {
        result.versions += versions.length;
        const latest = versions[0];
        if (latest === undefined || latest.isDeleteMarker) continue;
        const expired = rules.some((rule) => {
          const f = rule.filter;
          if (f?.prefix !== undefined && !key.startsWith(f.prefix)) return false;
          if (Object.entries(f?.tags ?? {}).some(([k, v]) => latest.tags[k] !== v)) return false;
          if (f?.sizeGreaterThan !== undefined && latest.size <= f.sizeGreaterThan) return false;
          if (f?.sizeLessThan !== undefined && latest.size >= f.sizeLessThan) return false;
          const days = rule.expiration?.days;
          const date = rule.expiration?.date;
          if (days !== undefined) return Date.parse(latest.lastModified) + days * day <= now;
          if (date !== undefined) return Date.parse(date) <= now;
          return false;
        });
        if (!expired) continue;
        if (state.bucket.versioning !== 'enabled' && lockOf(latest, ctx.now()) !== undefined) {
          result.locked += 1;
          continue;
        }
        const marker = storage.remove(state, key);
        if (marker) result.deleteMarkers += 1;
        else result.deleted += 1;
      }
      for (const [id, upload] of [...storage.uploads]) {
        if (upload.bucket !== bucket) continue;
        const abort = rules.find(
          (r) =>
            r.abortIncompleteUpload?.daysAfterInitiation !== undefined &&
            upload.key.startsWith(r.filter?.prefix ?? ''),
        );
        const days = abort?.abortIncompleteUpload?.daysAfterInitiation;
        if (days !== undefined && Date.parse(upload.initiated) + days * day <= now) {
          storage.uploads.delete(id);
          result.uploadsAborted += 1;
        }
      }
      return result;
    },
  };
}
