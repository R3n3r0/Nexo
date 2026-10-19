/**
 * The media rules of a bucket (media worker): video rules (rendition
 * presets, poster, thumbnails) and image rules (variants in every format
 * and width), their defaults and the validation the server applies to
 * each rule (domain.MediaConfig).
 */
import type { MediaImageFormat, MediaImages, MediaRendition, MediaRule } from '@/services';

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

/** The suffixes of a video rule that names none. */
export const defaultMediaSuffixes: readonly string[] = ['.mp4', '.mov', '.m4v'];

/** The suffixes of an image rule that names none: the images the worker decodes. */
export const defaultImageSuffixes: readonly string[] = [
  '.avif',
  '.bmp',
  '.gif',
  '.heic',
  '.heif',
  '.jpeg',
  '.jpg',
  '.png',
  '.tif',
  '.tiff',
  '.webp',
];

/** The formats of the image variants, the most compact first (domain.MediaImageFormats). */
export const mediaImageFormats: readonly MediaImageFormat[] = ['avif', 'webp', 'jpeg', 'png'];

/** How the formats are written for people. */
export const imageFormatLabels: Record<MediaImageFormat, string> = {
  avif: 'AVIF',
  webp: 'WebP',
  jpeg: 'JPEG',
  png: 'PNG',
};

/** The widths of an image rule that names none: thumbnails to large screens. */
export const defaultImageWidths: readonly number[] = [160, 320, 640, 1280, 1920, 2560];

/** The bounds of an image width, in pixels. */
export const minImageWidth = 16;
export const maxImageWidth = 8192;

/** At most this many widths per image rule. */
export const maxImageWidths = 16;

/** The id of the rule of the images option of bucket creation (domain.ImagesMediaRule). */
export const imagesMediaRuleId = 'images';

/** The rule of a bucket created with the images option: every format at every default width. */
export function imagesMediaRule(): MediaRule {
  return {
    id: imagesMediaRuleId,
    images: { formats: [...mediaImageFormats], widths: [...defaultImageWidths] },
  };
}

/** Whether a rule is an image rule. */
export function isImageRule(rule: MediaRule): boolean {
  return rule.images !== undefined;
}

/** The suffixes a rule selects: its own, or the default of its kind. */
export function mediaRuleSuffixes(rule: MediaRule): readonly string[] {
  if (rule.suffixes !== undefined && rule.suffixes.length > 0) return rule.suffixes;
  return isImageRule(rule) ? defaultImageSuffixes : defaultMediaSuffixes;
}

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
  | 'id'
  | 'idDuplicate'
  | 'prefixTooLong'
  | 'prefixReserved'
  | 'suffix'
  | 'interval'
  | 'nothing'
  | 'imageVideoOutputs'
  | 'imageFormat'
  | 'imageWidth'
  | 'imageWidths';

/** Whether a width is a whole number of pixels within the bounds. */
export function validImageWidth(width: number): boolean {
  return Number.isInteger(width) && width >= minImageWidth && width <= maxImageWidth;
}

/** The first problem of the variants of an image rule, as the server checks them. */
function imageIssue(images: MediaImages): MediaRuleIssue | undefined {
  if ((images.formats ?? []).some((f) => !mediaImageFormats.includes(f))) return 'imageFormat';
  const widths = images.widths ?? [];
  if (widths.some((w) => !validImageWidth(w))) return 'imageWidth';
  if (new Set(widths).size > maxImageWidths) return 'imageWidths';
  return undefined;
}

/**
 * The message the server gives for a problem of a rule (domain.MediaConfig
 * Validate), without the "media rule <id>:" lead.
 */
export function mediaRuleIssueMessage(rule: MediaRule, issue: MediaRuleIssue): string {
  switch (issue) {
    case 'id':
      return "the id must be 1-64 letters, digits, '.', '_' or '-'";
    case 'idDuplicate':
      return 'duplicate id';
    case 'prefixTooLong':
      return 'the prefix is longer than 1024 bytes';
    case 'prefixReserved':
      return `the prefix ${mediaSidecarPrefix} is reserved`;
    case 'suffix':
      return 'invalid suffix';
    case 'interval':
      return 'the thumbnail interval must be a duration from 1s to 10m';
    case 'nothing':
      return 'nothing to produce (renditions, poster or thumbnails)';
    case 'imageVideoOutputs':
      return 'an image rule has no renditions, poster or thumbnails';
    case 'imageFormat': {
      const format = (rule.images?.formats ?? []).find((f) => !mediaImageFormats.includes(f));
      return `unknown image format ${JSON.stringify(format ?? '')} (one of ${mediaImageFormats.join(', ')})`;
    }
    case 'imageWidth': {
      const width = (rule.images?.widths ?? []).find((w) => !validImageWidth(w));
      return `the image widths must be from ${String(minImageWidth)} to ${String(maxImageWidth)} pixels, not ${String(width)}`;
    }
    case 'imageWidths':
      return `at most ${String(maxImageWidths)} image widths`;
  }
}

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
  const videoOutputs =
    (rule.renditions ?? []).length > 0 || rule.poster === true || rule.thumbnails !== undefined;
  if (rule.images !== undefined) {
    if (videoOutputs) issues.push('imageVideoOutputs');
    const image = imageIssue(rule.images);
    if (image !== undefined) issues.push(image);
  } else if (!videoOutputs) {
    issues.push('nothing');
  }
  return issues;
}

/**
 * The rule as the server stores it: suffixes lowercased, sorted and
 * deduplicated (the default of its kind when none), renditions tallest
 * first, the interval of the thumbnails normalized; for an image rule the
 * formats the most compact first (all when none) and the widths ascending
 * and deduplicated (the defaults when none).
 */
export function normalizeMediaRule(rule: MediaRule): MediaRule {
  const given = mediaRuleSuffixes(rule);
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
  if (rule.images !== undefined) {
    const named = rule.images.formats ?? [];
    const widths = rule.images.widths?.length ? rule.images.widths : defaultImageWidths;
    out.images = {
      formats: mediaImageFormats.filter((f) => named.length === 0 || named.includes(f)),
      widths: [...new Set(widths)].sort((a, b) => a - b),
    };
  }
  return out;
}

/** The first rule selecting a key (the sidecars never match). */
export function matchMediaRule(rules: readonly MediaRule[], key: string): MediaRule | undefined {
  if (key.startsWith(mediaSidecarPrefix)) return undefined;
  const lower = key.toLowerCase();
  return rules.find((rule) => {
    if (!key.startsWith(rule.prefix ?? '')) return false;
    return mediaRuleSuffixes(rule).some((s) => lower.endsWith(s));
  });
}

/** The image types the media worker decodes (by content type). */
const imageTypes = /^image\/(avif|bmp|gif|heic|heif|jpeg|png|tiff|webp)\b/i;

/**
 * Whether an object is an image the media worker can derive variants
 * from: by its content type or by the default suffixes of an image rule.
 */
export function isMediaImage(key: string, contentType?: string): boolean {
  if (contentType !== undefined && imageTypes.test(contentType)) return true;
  const lower = key.toLowerCase();
  return defaultImageSuffixes.some((s) => lower.endsWith(s));
}
