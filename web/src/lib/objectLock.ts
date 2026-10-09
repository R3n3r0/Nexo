/**
 * Object Lock of a version in the console (D-OBJ-06, TS-U-12): what a
 * change of retention means, with the rules of the server
 * (object.checkRetentionChange): anything goes once the retention expired;
 * an active retention may be extended (same or later date, staying or
 * becoming COMPLIANCE); shortening or removing an active GOVERNANCE
 * retention needs the governance bypass; an active COMPLIANCE retention is
 * only extended.
 */
import type { ObjectRetention, RetentionMode } from '@/services';

/** What saving a retention would do. */
export interface RetentionChange {
  kind:
    | 'unchanged'
    /** A new retention on a version without an active one. */
    | 'set'
    /** Same or later date, same or stricter mode. */
    | 'extend'
    /** Shorter date or weaker mode of an active GOVERNANCE retention. */
    | 'reduce'
    /** Removal of the retention. */
    | 'remove'
    /** A change an active COMPLIANCE retention forbids. */
    | 'compliance'
    /** A retain-until date missing or not in the future. */
    | 'invalid';
  /** The change needs the governance bypass (s3:BypassGovernanceRetention). */
  needsBypass: boolean;
}

/** True when a retention protects the version at now. */
export function isRetentionActive(retention: ObjectRetention, now: Date): boolean {
  return (
    retention.mode !== undefined &&
    retention.retainUntil !== undefined &&
    Date.parse(retention.retainUntil) > now.getTime()
  );
}

/** Classifies the change of a retention into mode/until at now. */
export function retentionChange(
  current: ObjectRetention,
  mode: RetentionMode | undefined,
  until: Date | null,
  now: Date,
): RetentionChange {
  const active = isRetentionActive(current, now);
  const result = (kind: RetentionChange['kind'], needsBypass = false): RetentionChange => ({
    kind,
    needsBypass,
  });
  if (mode === undefined) {
    if (current.mode === undefined) return result('unchanged');
    if (!active) return result('remove');
    return current.mode === 'COMPLIANCE' ? result('compliance') : result('remove', true);
  }
  if (until === null || Number.isNaN(until.getTime()) || until.getTime() <= now.getTime()) {
    return result('invalid');
  }
  const currentUntil =
    current.retainUntil !== undefined ? Date.parse(current.retainUntil) : Number.NaN;
  if (current.mode === mode && currentUntil === until.getTime()) {
    return result('unchanged');
  }
  if (!active) return result('set');
  const extends_ = until.getTime() >= currentUntil;
  if (current.mode === 'COMPLIANCE') {
    return extends_ && mode === 'COMPLIANCE' ? result('extend') : result('compliance');
  }
  return extends_ ? result('extend') : result('reduce', true);
}

/** Why a version may not be deleted at now, if it may not. */
export type DeleteBlock = 'legalHold' | 'compliance' | 'governance';

/**
 * Whether a version under retention or legal hold may be deleted (the
 * rules of object.checkDeletable): never under legal hold or an active
 * COMPLIANCE retention, under an active GOVERNANCE retention only with the
 * bypass.
 */
export function deleteBlock(
  retention: ObjectRetention | undefined,
  legalHold: 'ON' | 'OFF' | undefined,
  now: Date,
): DeleteBlock | undefined {
  if (legalHold === 'ON') return 'legalHold';
  if (retention !== undefined && isRetentionActive(retention, now)) {
    return retention.mode === 'COMPLIANCE' ? 'compliance' : 'governance';
  }
  return undefined;
}
