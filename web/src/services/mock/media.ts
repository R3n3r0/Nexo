import {
  matchMediaRule,
  maxMediaRules,
  mediaRenditions,
  normalizeMediaRule,
  parseGoDuration,
  validateMediaRule,
  type MediaRuleIssue,
} from '@/lib/mediaRules';

import type { RuleProblem } from '../errors';
import type { MediaService } from '../interfaces';
import type { MediaRendition, MediaStatus } from '../types';
import type { MockStorage } from './storage';
import { clone, invalidConfiguration, problem } from './support';

/** Messages of the problems of a rule, as the server words them. */
const messages: Record<MediaRuleIssue, string> = {
  id: "the id must be 1-64 letters, digits, '.', '_' or '-'",
  idDuplicate: 'duplicate rule id',
  prefixTooLong: 'the prefix is longer than 1024 bytes',
  prefixReserved: 'the prefix .nexo-media/ is reserved',
  suffix: 'invalid suffix',
  interval: 'the thumbnail interval must be a duration from 1s to 10m',
  nothing: 'nothing to produce (renditions, poster or thumbnails)',
};

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

const statusKey = (key: string, versionId: string) => `${key}\u0000${versionId}`;

/**
 * Video rules of the mock buckets. The demo has no media worker: a
 * reprocessed video is done at once, with the outputs its rule asks for.
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
            message: `media rule ${JSON.stringify(rule.id)}: ${messages[issue]}`,
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
      state.mediaStatus ??= new Map();
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
