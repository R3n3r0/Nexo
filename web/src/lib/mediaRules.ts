/**
 * The video rules of a bucket (media worker): rendition presets, defaults
 * and the validation the server applies to each rule (domain.MediaConfig).
 */
import type { MediaRendition, MediaRule } from '@/services';

import { mediaSidecarPrefix } from './media';

/** The rendition presets, tallest first. */
export const mediaRenditions: readonly MediaRendition[] = [
  '2160p',
  '1440p',
  '1080p',
  '720p',
  '480p',
  '360p',
  '240p',
];

/** The suffixes of a rule that names none. */
export const defaultMediaSuffixes: readonly string[] = ['.mp4', '.mov', '.m4v'];

/** The interval of the thumbnails of a rule that names none. */
export const defaultThumbnailInterval = '10s';

/** At most this many rules per bucket. */
export const maxMediaRules = 100;

const ruleId = /^[A-Za-z0-9._-]{1,64}$/;

const unitNs: Record<string, number> = {
  ns: 1,
  us: 1e3,
  µs: 1e3,
  μs: 1e3,
  ms: 1e6,
  s: 1e9,
  m: 60e9,
  h: 3600e9,
};

/**
 * Parses a Go duration ("10s", "1m30s", "1.5m") into seconds; undefined
 * when it is not one.
 */
export function parseGoDuration(text: string): number | undefined {
  const value = text.trim();
  if (value === '0') return 0;
  const re = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/gy;
  let total = 0;
  let consumed = 0;
  for (let match = re.exec(value); match !== null; match = re.exec(value)) {
    const [whole, amount = '', unit = ''] = match;
    total += Number(amount) * (unitNs[unit] ?? 0);
    consumed += whole.length;
  }
  if (value === '' || consumed !== value.length) return undefined;
  return total / 1e9;
}

/** Formats whole seconds as Go does (time.Duration.String: "10s", "1m30s"). */
export function formatGoDuration(seconds: number): string {
  if (seconds === 0) return '0s';
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  if (h > 0) return `${String(h)}h${String(m)}m${String(s)}s`;
  if (m > 0) return `${String(m)}m${String(s)}s`;
  return `${String(s)}s`;
}

/** Problems of a rule (translated under bucketMedia.issues.*). */
export type MediaRuleIssue =
  'id' | 'idDuplicate' | 'prefixTooLong' | 'prefixReserved' | 'suffix' | 'interval' | 'nothing';

/**
 * Validates a rule as the server does; others are the ids of the other
 * rules of the bucket (duplicates).
 */
export function validateMediaRule(
  rule: MediaRule,
  others: readonly string[] = [],
): MediaRuleIssue[] {
  const issues: MediaRuleIssue[] = [];
  if (!ruleId.test(rule.id)) issues.push('id');
  else if (others.includes(rule.id)) issues.push('idDuplicate');
  const prefix = rule.prefix ?? '';
  if (new TextEncoder().encode(prefix).length > 1024) issues.push('prefixTooLong');
  if (prefix.startsWith(mediaSidecarPrefix)) issues.push('prefixReserved');
  if ((rule.suffixes ?? []).some((s) => s.trim() === '' || s.trim().length > 32)) {
    issues.push('suffix');
  }
  if (rule.thumbnails !== undefined) {
    const seconds = parseGoDuration(rule.thumbnails.interval ?? defaultThumbnailInterval);
    if (seconds === undefined || seconds < 1 || seconds > 600) issues.push('interval');
  }
  if (
    (rule.renditions ?? []).length === 0 &&
    rule.poster !== true &&
    rule.thumbnails === undefined
  ) {
    issues.push('nothing');
  }
  return issues;
}

/**
 * The rule as the server stores it: suffixes lowercased, sorted and
 * deduplicated (the default when none), renditions tallest first, the
 * interval of the thumbnails normalized.
 */
export function normalizeMediaRule(rule: MediaRule): MediaRule {
  const given =
    rule.suffixes !== undefined && rule.suffixes.length > 0 ? rule.suffixes : defaultMediaSuffixes;
  const suffixes = [...new Set(given.map((s) => s.trim().toLowerCase()))].sort();
  const renditions = mediaRenditions.filter((r) => rule.renditions?.includes(r) === true);
  const out: MediaRule = { id: rule.id, suffixes };
  if (rule.prefix !== undefined && rule.prefix !== '') out.prefix = rule.prefix;
  if (renditions.length > 0) out.renditions = renditions;
  if (rule.poster === true) out.poster = true;
  if (rule.thumbnails !== undefined) {
    const seconds = parseGoDuration(rule.thumbnails.interval ?? defaultThumbnailInterval) ?? 10;
    out.thumbnails = { interval: formatGoDuration(Math.round(seconds)) };
  }
  return out;
}

/** The first rule selecting a key (the sidecars never match). */
export function matchMediaRule(rules: readonly MediaRule[], key: string): MediaRule | undefined {
  if (key.startsWith(mediaSidecarPrefix)) return undefined;
  const lower = key.toLowerCase();
  return rules.find((rule) => {
    if (!key.startsWith(rule.prefix ?? '')) return false;
    const suffixes = rule.suffixes?.length ? rule.suffixes : defaultMediaSuffixes;
    return suffixes.some((s) => lower.endsWith(s));
  });
}
