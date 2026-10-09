/**
 * Event notifications in the console (D-BKT-04): the event names a
 * subscription may name and the parts of a target ARN.
 */
import type { EventSubscription, NotifyTarget } from '@/services';

/** The event families and names a bucket subscription may subscribe to. */
export const eventGroups = [
  {
    family: 's3:ObjectCreated:*',
    events: [
      's3:ObjectCreated:Put',
      's3:ObjectCreated:Post',
      's3:ObjectCreated:Copy',
      's3:ObjectCreated:CompleteMultipartUpload',
    ],
  },
  {
    family: 's3:ObjectRemoved:*',
    events: ['s3:ObjectRemoved:Delete', 's3:ObjectRemoved:DeleteMarkerCreated'],
  },
  { family: 's3:ObjectAccessed:*', events: ['s3:ObjectAccessed:Get', 's3:ObjectAccessed:Head'] },
  { family: 's3:ObjectTagging:*', events: ['s3:ObjectTagging:Put', 's3:ObjectTagging:Delete'] },
  {
    family: 's3:LifecycleExpiration:*',
    events: ['s3:LifecycleExpiration:Delete', 's3:LifecycleExpiration:DeleteMarkerCreated'],
  },
] as const;

/** Every event name (and family) known to the server. */
export const knownEvents: readonly string[] = eventGroups.flatMap((g) => [g.family, ...g.events]);

/** The id and type of a target ARN (arn:nexo:sqs:<region>:<id>:<type>). */
export function parseTargetArn(arn: string): { id: string; type: string } | undefined {
  const parts = arn.split(':');
  if (parts.length !== 6 || parts[0] !== 'arn' || parts[2] !== 'sqs') {
    return undefined;
  }
  const [, , , , id, type] = parts;
  if (id === undefined || id === '' || type === undefined || type === '') {
    return undefined;
  }
  return { id, type };
}

/** "<id>:<type>" of a target: what POST /notify/targets/{id}/test accepts. */
export function targetRef(target: Pick<NotifyTarget, 'id' | 'type'>): string {
  return `${target.id}:${target.type}`;
}

/** Problems of a subscription (translated under bucketEvents.issues.*). */
export type SubscriptionIssue = 'arn' | 'unknownTarget' | 'events' | 'unknownEvent' | 'idTooLong';

/**
 * Validates a subscription as the server does: a target ARN (one of the
 * configured targets when they are known), known events, an id of at most
 * 255 characters.
 */
export function validateSubscription(
  sub: EventSubscription,
  targets?: readonly NotifyTarget[],
): SubscriptionIssue[] {
  const issues: SubscriptionIssue[] = [];
  if (parseTargetArn(sub.arn) === undefined) {
    issues.push('arn');
  } else if (targets !== undefined && !targets.some((t) => t.arn === sub.arn)) {
    issues.push('unknownTarget');
  }
  if (sub.events.length === 0) {
    issues.push('events');
  } else if (sub.events.some((e) => !knownEvents.includes(e))) {
    issues.push('unknownEvent');
  }
  if ((sub.id ?? '').length > 255) {
    issues.push('idTooLong');
  }
  return issues;
}

/**
 * The events of a selection without the names a selected family already
 * covers ("s3:ObjectCreated:*" makes "s3:ObjectCreated:Put" redundant).
 */
export function normalizeEvents(events: readonly string[]): string[] {
  const families = events.filter((e) => e.endsWith(':*'));
  return events.filter(
    (e, i) =>
      events.indexOf(e) === i &&
      (e.endsWith(':*') || !families.some((f) => e.startsWith(f.slice(0, -1)))),
  );
}
