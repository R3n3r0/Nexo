import { describe, expect, it } from 'vitest';

import type { ObjectRetention, RetentionMode } from '@/services';

import { deleteBlock, retentionChange, type RetentionChange } from './objectLock';

const now = new Date('2026-10-09T12:00:00Z');
const at = (days: number) => new Date(now.getTime() + days * 86_400_000);
const retention = (mode: RetentionMode, days: number): ObjectRetention => ({
  mode,
  retainUntil: at(days).toISOString(),
});

/** TS-U-12 on the console side: governance/compliance × change × bypass. */
describe('retentionChange', () => {
  it.each<[string, ObjectRetention, RetentionMode | undefined, Date | null, RetentionChange]>([
    ['nothing to nothing', {}, undefined, null, { kind: 'unchanged', needsBypass: false }],
    ['a new retention', {}, 'GOVERNANCE', at(10), { kind: 'set', needsBypass: false }],
    ['a date in the past', {}, 'COMPLIANCE', at(-1), { kind: 'invalid', needsBypass: false }],
    ['no date', {}, 'COMPLIANCE', null, { kind: 'invalid', needsBypass: false }],
    [
      'the same retention',
      retention('GOVERNANCE', 10),
      'GOVERNANCE',
      at(10),
      { kind: 'unchanged', needsBypass: false },
    ],
    [
      'governance extended',
      retention('GOVERNANCE', 10),
      'GOVERNANCE',
      at(20),
      { kind: 'extend', needsBypass: false },
    ],
    [
      'governance to compliance',
      retention('GOVERNANCE', 10),
      'COMPLIANCE',
      at(10),
      { kind: 'extend', needsBypass: false },
    ],
    [
      'governance shortened',
      retention('GOVERNANCE', 10),
      'GOVERNANCE',
      at(5),
      { kind: 'reduce', needsBypass: true },
    ],
    [
      'governance removed',
      retention('GOVERNANCE', 10),
      undefined,
      null,
      { kind: 'remove', needsBypass: true },
    ],
    [
      'compliance extended',
      retention('COMPLIANCE', 10),
      'COMPLIANCE',
      at(20),
      { kind: 'extend', needsBypass: false },
    ],
    [
      'compliance shortened',
      retention('COMPLIANCE', 10),
      'COMPLIANCE',
      at(5),
      { kind: 'compliance', needsBypass: false },
    ],
    [
      'compliance weakened',
      retention('COMPLIANCE', 10),
      'GOVERNANCE',
      at(20),
      { kind: 'compliance', needsBypass: false },
    ],
    [
      'compliance removed',
      retention('COMPLIANCE', 10),
      undefined,
      null,
      { kind: 'compliance', needsBypass: false },
    ],
    [
      'an expired retention removed',
      retention('COMPLIANCE', -1),
      undefined,
      null,
      { kind: 'remove', needsBypass: false },
    ],
    [
      'an expired retention replaced',
      retention('COMPLIANCE', -1),
      'GOVERNANCE',
      at(1),
      { kind: 'set', needsBypass: false },
    ],
  ])('%s', (_, current, mode, until, expected) => {
    expect(retentionChange(current, mode, until, now)).toEqual(expected);
  });
});

describe('deleteBlock', () => {
  it.each<
    [string, ObjectRetention | undefined, 'ON' | 'OFF' | undefined, ReturnType<typeof deleteBlock>]
  >([
    ['no lock', undefined, undefined, undefined],
    ['legal hold', undefined, 'ON', 'legalHold'],
    ['legal hold off', {}, 'OFF', undefined],
    ['compliance', retention('COMPLIANCE', 1), 'OFF', 'compliance'],
    ['governance', retention('GOVERNANCE', 1), undefined, 'governance'],
    ['expired retention', retention('COMPLIANCE', -1), 'OFF', undefined],
    ['legal hold wins', retention('GOVERNANCE', 1), 'ON', 'legalHold'],
  ])('%s', (_, r, hold, expected) => {
    expect(deleteBlock(r, hold, now)).toBe(expected);
  });
});
