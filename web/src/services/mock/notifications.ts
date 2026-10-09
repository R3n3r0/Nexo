import { parseTargetArn, validateSubscription } from '@/lib/events';

import type { RuleProblem } from '../errors';
import type { NotificationService } from '../interfaces';
import type { NotifyTarget } from '../types';
import { sampleNotifyTargets, type MockStorage } from './storage';
import { clone, invalidConfiguration, problem } from './support';

export interface MockNotificationOptions {
  /** The configured targets (sampleNotifyTargets() by default). */
  notifyTargets?: NotifyTarget[];
}

const issueMessages: Record<string, { code: string; message: string }> = {
  arn: { code: 'InvalidArgument', message: 'The ARN is not valid.' },
  unknownTarget: { code: 'InvalidArgument', message: 'The ARN does not name a configured target.' },
  events: { code: 'InvalidArgument', message: 'A subscription needs at least one event.' },
  unknownEvent: { code: 'InvalidArgument', message: 'The event name is not supported.' },
  idTooLong: { code: 'InvalidArgument', message: 'The id must be at most 255 characters.' },
};

/** Event subscriptions of the mock buckets and the configured targets. */
export function createMockNotificationService(
  storage: MockStorage,
  options: MockNotificationOptions = {},
): NotificationService {
  const { ctx } = storage;
  const targets = clone(options.notifyTargets ?? sampleNotifyTargets());
  let generated = 0;

  function find(ref: string): NotifyTarget {
    const parsed = parseTargetArn(ref);
    const [id, type] = parsed ? [parsed.id, parsed.type] : ref.split(':');
    const target = targets.find((t) => t.id === id && t.type === type);
    if (!target) {
      throw problem(404, 'not_found', `Target ${ref} does not exist.`);
    }
    return target;
  }

  return {
    async get(bucket) {
      await ctx.delay();
      return { subscriptions: clone(storage.bucket(bucket).notifications ?? []) };
    },
    async set(bucket, config) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const problems: RuleProblem[] = [];
      config.subscriptions.forEach((sub, i) => {
        const issue = validateSubscription(sub, targets)[0];
        if (issue !== undefined) {
          problems.push({
            rule: i + 1,
            ...(sub.id !== undefined ? { id: sub.id } : {}),
            ...(issueMessages[issue] ?? { code: 'InvalidArgument', message: issue }),
          });
        }
      });
      if (problems.length > 0) {
        throw invalidConfiguration(problems);
      }
      const subscriptions = config.subscriptions.map((sub) => {
        const out = clone(sub);
        if (out.id === undefined || out.id === '') {
          generated += 1;
          out.id = `nexo-subscription-${String(generated)}`;
        }
        return out;
      });
      if (subscriptions.length === 0) {
        delete state.notifications;
      } else {
        state.notifications = subscriptions;
      }
      return { subscriptions: clone(subscriptions) };
    },
    async remove(bucket) {
      await ctx.delay();
      delete storage.bucket(bucket).notifications;
    },
    async targets() {
      await ctx.delay();
      return clone(targets);
    },
    async test(ref) {
      await ctx.delay();
      const target = find(ref);
      const name = `${target.id}:${target.type}`;
      if (target.online && target.enabled) {
        return { target: name, delivered: true };
      }
      return {
        target: name,
        delivered: false,
        error: target.lastError ?? 'The target is offline.',
      };
    },
  };
}
