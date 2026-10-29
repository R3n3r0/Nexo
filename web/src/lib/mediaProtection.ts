/**
 * The download protection of the videos of a bucket
 * (docs/proposals/video-protection.md): its defaults, its bounds and the
 * validation the server applies (domain.MediaProtection Validate).
 */
import type { MediaProtection } from '@/services';

import { mediaSidecarPrefix } from './media';
import { defaultMediaSuffixes } from './mediaRules';

/** Longest validity of a playback token by default: two hours. */
export const defaultTokenTtlSeconds = 7200;
export const minTokenTtlSeconds = 60;
export const maxTokenTtlSeconds = 86_400;
/** Simultaneous streams of a viewer by default. */
export const defaultMaxSessions = 2;
export const maxMaxSessions = 100;
/** maxSessions of a protection without a limit of streams. */
export const unlimitedSessions = -1;
/** Keys served at most this many times faster than playback by default. */
export const defaultPaceFactor = 2;
export const maxPaceFactor = 20;
export const maxAllowedOrigins = 32;

/** The protection of a bucket that never set one. */
export function defaultMediaProtection(): MediaProtection {
  return {
    enabled: false,
    suffixes: [...defaultMediaSuffixes],
    tokenTtlSeconds: defaultTokenTtlSeconds,
    maxSessions: defaultMaxSessions,
    paceFactor: defaultPaceFactor,
    watermark: true,
    allowedOrigins: [],
    shareLinks: false,
    mode: 'standard',
    legacyDevices: 'refuse',
    forensic: false,
  };
}

export type MediaProtectionIssue =
  | 'prefixTooLong'
  | 'prefixReserved'
  | 'suffix'
  | 'tokenTtl'
  | 'maxSessions'
  | 'paceFactor'
  | 'origins'
  | 'origin';

const origin = /^(https?):\/\/([^/?#@\s]+)\/?$/i;

/** A web origin (http(s)://host[:port], no path) lowered; undefined when it is not one. */
export function normalizeOrigin(text: string): string | undefined {
  const match = origin.exec(text.trim());
  return match === null ? undefined : `${match[1]!}://${match[2]!}`.toLowerCase();
}

/** Whether a value is 0 (the default) or a whole number in [min, max]. */
function inRange(value: number | undefined, min: number, max: number): boolean {
  const v = value ?? 0;
  return v === 0 || (Number.isInteger(v) && v >= min && v <= max);
}

/** Validates a protection as the server does (0 or absent picks the default). */
export function validateMediaProtection(p: MediaProtection): MediaProtectionIssue[] {
  const issues: MediaProtectionIssue[] = [];
  const prefix = p.prefix ?? '';
  if (new TextEncoder().encode(prefix).length > 1024) issues.push('prefixTooLong');
  if (prefix.startsWith(mediaSidecarPrefix)) issues.push('prefixReserved');
  if ((p.suffixes ?? []).some((s) => s.trim() === '' || s.trim().length > 32)) {
    issues.push('suffix');
  }
  if (!inRange(p.tokenTtlSeconds, minTokenTtlSeconds, maxTokenTtlSeconds)) issues.push('tokenTtl');
  if (p.maxSessions !== unlimitedSessions && !inRange(p.maxSessions, 1, maxMaxSessions)) {
    issues.push('maxSessions');
  }
  if (!inRange(p.paceFactor, 1, maxPaceFactor)) issues.push('paceFactor');
  if (p.allowedOrigins.length > maxAllowedOrigins) issues.push('origins');
  else if (p.allowedOrigins.some((o) => normalizeOrigin(o) === undefined)) issues.push('origin');
  return issues;
}

/** The message the server gives for a problem of a protection. */
export function mediaProtectionIssueMessage(
  p: MediaProtection,
  issue: MediaProtectionIssue,
): string {
  const lead = 'media protection: ';
  switch (issue) {
    case 'prefixTooLong':
      return `${lead}the prefix is longer than 1024 bytes`;
    case 'prefixReserved':
      return `${lead}the prefix ${mediaSidecarPrefix} is reserved`;
    case 'suffix': {
      const bad = (p.suffixes ?? []).find((s) => s.trim() === '' || s.trim().length > 32);
      return `${lead}invalid suffix ${JSON.stringify(bad ?? '')}`;
    }
    case 'tokenTtl':
      return `${lead}the token TTL must be ${String(minTokenTtlSeconds)} to ${String(maxTokenTtlSeconds)} seconds`;
    case 'maxSessions':
      return `${lead}maxSessions must be 1 to ${String(maxMaxSessions)}, or -1 for no limit`;
    case 'paceFactor':
      return `${lead}paceFactor must be 1 to ${String(maxPaceFactor)}`;
    case 'origins':
      return `${lead}at most ${String(maxAllowedOrigins)} allowed origins`;
    case 'origin': {
      const bad = p.allowedOrigins.find((o) => normalizeOrigin(o) === undefined);
      return `${lead}${JSON.stringify(bad ?? '')} is not an origin (https://host[:port])`;
    }
  }
}

/**
 * The protection as the server stores it: the defaults filled, the
 * suffixes and the origins lowered. Expects a valid protection.
 */
export function normalizeMediaProtection(p: MediaProtection): MediaProtection {
  const suffixes = p.suffixes?.length ? p.suffixes : defaultMediaSuffixes;
  return {
    enabled: p.enabled,
    ...(p.prefix ? { prefix: p.prefix } : {}),
    suffixes: suffixes.map((s) => s.trim().toLowerCase()),
    tokenTtlSeconds: p.tokenTtlSeconds || defaultTokenTtlSeconds,
    maxSessions: p.maxSessions || defaultMaxSessions,
    paceFactor: p.paceFactor || defaultPaceFactor,
    watermark: p.watermark,
    allowedOrigins: p.allowedOrigins.map((o) => normalizeOrigin(o) ?? o),
    shareLinks: p.shareLinks,
    mode: p.mode ?? 'standard',
    legacyDevices: p.legacyDevices ?? 'refuse',
    forensic: p.forensic,
  };
}
