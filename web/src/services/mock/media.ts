import {
  defaultImageWidths,
  matchMediaRule,
  maxMediaRules,
  mediaImageFormats,
  mediaRenditions,
  mediaRuleIssueMessage,
  normalizeMediaRule,
  parseGoDuration,
  validateMediaRule,
} from '@/lib/mediaRules';

import type { RuleProblem } from '../errors';
import type { MediaService } from '../interfaces';
import type {
  MediaImageFormat,
  MediaImageOutput,
  MediaRendition,
  MediaRule,
  MediaStatus,
} from '../types';
import { dataUrl } from './objects';
import type { MockStorage, MockVersion } from './storage';
import { clone, invalidConfiguration, problem } from './support';

/** Height and bandwidth of the presets (domain.MediaRenditionPreset). */
const presets: Record<MediaRendition, { height: number; bandwidth: number }> = {
  '2160p': { height: 2160, bandwidth: 16_128_000 },
  '1440p': { height: 1440, bandwidth: 8_128_000 },
  '1080p': { height: 1080, bandwidth: 5_128_000 },
  '720p': { height: 720, bandwidth: 2_928_000 },
  '480p': { height: 480, bandwidth: 1_496_000 },
  '360p': { height: 360, bandwidth: 896_000 },
  '240p': { height: 240, bandwidth: 464_000 },
};

/** The source every mock video pretends to be: 1080p, two minutes. */
const source = { width: 1920, height: 1080, duration: 120 };

/** The size every mock image pretends to be. */
const imageSource = { width: 1600, height: 1200 };

/** Bytes per pixel of each format, for plausible variant sizes. */
const bytesPerPixel: Record<MediaImageFormat, number> = {
  avif: 0.06,
  webp: 0.1,
  jpeg: 0.18,
  png: 1.2,
};

/** The variants of an image as the worker derives them (the URL is the Admin API route). */
function imageVariants(
  bucket: string,
  key: string,
  version: MockVersion,
  rule: MediaRule,
): MediaImageOutput[] {
  const formats = rule.images?.formats?.length ? rule.images.formats : mediaImageFormats;
  const widths = (rule.images?.widths?.length ? rule.images.widths : defaultImageWidths).filter(
    (w) => w < imageSource.width,
  );
  // The demo serves the original as every variant when it is small enough.
  const demo = dataUrl(version);
  return formats.flatMap((format) =>
    [...widths, 0].map((width) => {
      const pixelWidth = width === 0 ? imageSource.width : width;
      const pixelHeight = Math.round((imageSource.height * pixelWidth) / imageSource.width);
      const query = new URLSearchParams({ format, key });
      if (width > 0) query.set('w', String(width));
      if (version.versionId !== 'null') query.set('versionId', version.versionId);
      return {
        format,
        width,
        pixelWidth,
        pixelHeight,
        size: Math.round(pixelWidth * pixelHeight * bytesPerPixel[format]),
        contentType: `image/${format}`,
        url:
          demo !== '#'
            ? demo
            : `/api/v1/buckets/${encodeURIComponent(bucket)}/objects/image?${query.toString()}`,
      };
    }),
  );
}

const statusKey = (key: string, versionId: string) => `${key}\u0000${versionId}`;

/**
 * Media rules of the mock buckets. The demo has no media worker: a
 * reprocessed video or image is done at once, with the outputs its rule
 * asks for.
 */
export function createMockMediaService(storage: MockStorage): MediaService {
  const { ctx } = storage;

  function versionOf(bucket: string, key: string, versionId?: string) {
    const versions = storage.bucket(bucket).keys.get(key) ?? [];
    const version =
      versionId === undefined || versionId === ''
        ? versions.find((v) => !v.isDeleteMarker)
        : versions.find((v) => v.versionId === versionId);
    if (version === undefined || version.isDeleteMarker) {
      throw problem(404, 'object_not_found', `Object ${key} does not exist.`);
    }
    return version;
  }

  return {
    async get(bucket) {
      await ctx.delay();
      return { rules: clone(storage.bucket(bucket).media ?? []) };
    },
    async set(bucket, config) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const problems: RuleProblem[] = [];
      if (config.rules.length > maxMediaRules) {
        throw invalidConfiguration([
          {
            rule: 0,
            code: 'InvalidArgument',
            message: `at most ${String(maxMediaRules)} media rules`,
          },
        ]);
      }
      config.rules.forEach((rule, i) => {
        const others = config.rules.slice(0, i).map((r) => r.id);
        const issue = validateMediaRule(rule, others)[0];
        if (issue !== undefined) {
          problems.push({
            rule: i + 1,
            id: rule.id,
            code: 'InvalidArgument',
            message: `media rule ${JSON.stringify(rule.id)}: ${mediaRuleIssueMessage(rule, issue)}`,
          });
        }
      });
      if (problems.length > 0) {
        throw invalidConfiguration(problems);
      }
      const rules = config.rules.map(normalizeMediaRule);
      if (rules.length === 0) {
        delete state.media;
      } else {
        state.media = rules;
      }
      return { rules: clone(rules) };
    },
    async remove(bucket) {
      await ctx.delay();
      delete storage.bucket(bucket).media;
    },
    async reprocess(bucket, { key, versionId }) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const version = versionOf(bucket, key, versionId);
      const rule = matchMediaRule(state.media ?? [], key);
      if (rule === undefined) {
        throw problem(400, 'invalid_argument', `No media rule of ${bucket} selects ${key}.`);
      }
      state.mediaStatus ??= new Map();
      if (rule.images !== undefined) {
        state.mediaStatus.set(statusKey(key, version.versionId), {
          state: 'done',
          current: true,
          versionId: version.versionId,
          rule: rule.id,
          width: imageSource.width,
          height: imageSource.height,
          images: imageVariants(bucket, key, version, rule),
          // PNG and GIF sources pretend to have transparency.
          ...(/\.(png|gif)$/i.test(key) ? { alpha: true } : {}),
          updated: ctx.nowIso(),
        });
        return;
      }
      const renditions = mediaRenditions
        .filter((r) => rule.renditions?.includes(r) === true)
        .filter((r) => presets[r].height <= source.height)
        .map((name) => {
          const { height, bandwidth } = presets[name];
          return {
            name,
            height,
            width: Math.round((source.width * height) / source.height),
            bandwidth,
          };
        });
      const interval = rule.thumbnails
        ? (parseGoDuration(rule.thumbnails.interval ?? '10s') ?? 10)
        : 0;
      const status: MediaStatus = {
        state: 'done',
        current: true,
        versionId: version.versionId,
        rule: rule.id,
        duration: source.duration,
        width: source.width,
        height: source.height,
        ...(renditions.length > 0 ? { renditions } : {}),
        ...(rule.poster === true ? { poster: true } : {}),
        ...(rule.thumbnails
          ? {
              thumbnails: {
                interval,
                width: 160,
                height: 90,
                columns: 10,
                count: Math.ceil(source.duration / interval),
              },
            }
          : {}),
        updated: ctx.nowIso(),
      };
      state.mediaStatus.set(statusKey(key, version.versionId), status);
    },
    async status(bucket, key, versionId) {
      await ctx.delay();
      const version = versionOf(bucket, key, versionId);
      const found = storage.bucket(bucket).mediaStatus?.get(statusKey(key, version.versionId));
      return found ? clone(found) : { state: 'none', current: false };
    },
  };
}
