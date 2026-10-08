import type {
  AccessPreset,
  AccessRule,
  Bucket,
  BucketEncryption,
  BucketObjectLock,
  ObjectEntry,
  TagSet,
  Upload,
} from '../types';
import { clone, problem, type MockContext } from './support';

/** Content of a mock object: text/bytes for previews, or an uploaded blob. */
export type MockContent = Uint8Array | Blob;

/** One version (or delete marker) of a key. */
export interface MockVersion {
  key: string;
  versionId: string;
  size: number;
  etag: string;
  lastModified: string;
  contentType: string;
  isDeleteMarker: boolean;
  tags: TagSet;
  userMetadata: Record<string, string>;
  content?: MockContent;
}

/** Seed of a mock object (its latest version). */
export interface MockObjectSeed {
  key: string;
  size?: number;
  contentType?: string;
  /** Text content (UTF-8), used by the previews of the demo mode. */
  content?: string;
  lastModified?: string;
  tags?: TagSet;
}

/** Settings of a mock bucket besides the Bucket document. */
export interface MockBucketState {
  bucket: Bucket;
  /** Versions per key, newest first. */
  keys: Map<string, MockVersion[]>;
  policy: string | null;
  rules: AccessRule[];
  encryption: BucketEncryption | null;
  /** Default retention set with setObjectLock (sample default otherwise). */
  objectLock?: BucketObjectLock;
}

/** A pending multipart upload. */
export interface MockUploadState {
  bucket: string;
  key: string;
  uploadId: string;
  initiated: string;
  contentType: string;
  userMetadata: Record<string, string>;
  parts: Map<number, { etag: string; size: number; body: Blob; lastModified: string }>;
}

export type UploadLimits = Pick<Upload, 'partSize' | 'minPartSize' | 'maxPartSize' | 'maxParts'>;

export interface MockStorageOptions {
  buckets?: Bucket[];
  /**
   * Objects per bucket (latest versions). Defaults to sampleObjects() when
   * the sample buckets are used.
   */
  objects?: Record<string, MockObjectSeed[]>;
  /** Part limits announced by the mock multipart uploads. */
  uploadLimits?: Partial<UploadLimits>;
}

/** Shared in-memory state of the storage mocks (buckets, objects, uploads). */
export interface MockStorage {
  readonly ctx: MockContext;
  readonly buckets: Map<string, MockBucketState>;
  readonly uploads: Map<string, MockUploadState>;
  readonly uploadLimits: UploadLimits;
  /** The bucket or a 404 bucket_not_found. */
  bucket(name: string): MockBucketState;
  /** Monotonic id generator (version ids, upload ids, etags). */
  nextId(prefix: string): string;
  /** Stores a new version, following the versioning state of the bucket. */
  put(
    state: MockBucketState,
    version: Omit<MockVersion, 'versionId' | 'isDeleteMarker'>,
  ): MockVersion;
  /**
   * Deletes a key (delete marker when versioning is enabled) or one of its
   * versions. Returns the delete marker created, if any.
   */
  remove(state: MockBucketState, key: string, versionId?: string): MockVersion | undefined;
}

/** A 1x1 transparent PNG, for the image previews of the demo mode. */
const tinyPng = Uint8Array.from(
  atob(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
  ),
  (c) => c.charCodeAt(0),
);

/** Objects of the sample buckets: folders, unicode and special characters. */
export function sampleObjects(): Record<string, MockObjectSeed[]> {
  return {
    media: [
      { key: 'photos/2026/beach.png', contentType: 'image/png', size: tinyPng.byteLength },
      { key: 'photos/2026/città di notte.png', contentType: 'image/png', size: 2_400_000 },
      { key: 'photos/2026/', size: 0, contentType: 'application/x-directory' },
      { key: 'videos/intro.mp4', contentType: 'video/mp4', size: 48_000_000 },
      { key: 'docs/report #1.pdf', contentType: 'application/pdf', size: 820_000 },
      {
        key: 'docs/100% done.txt',
        contentType: 'text/plain',
        content: 'Everything is uploaded.\nTutto caricato.\n',
      },
      {
        key: 'catalog.json',
        contentType: 'application/json',
        content: '{"items":[{"id":1,"name":"beach"},{"id":2,"name":"città"}]}',
      },
      { key: 'music/canción.mp3', contentType: 'audio/mpeg', size: 5_200_000 },
    ],
    backups: [
      { key: 'db/2026-10-01.sql.gz', contentType: 'application/gzip', size: 9_800_000_000 },
      { key: 'db/2026-10-02.sql.gz', contentType: 'application/gzip', size: 9_900_000_000 },
      { key: 'etc/nexo.env', contentType: 'text/plain', content: 'NEXO_REGION=us-east-1\n' },
    ],
    'logs-archive': [
      { key: '2026/10/01/app.log', contentType: 'text/plain', content: 'started\nready\n' },
    ],
  };
}

const defaultLimits: UploadLimits = {
  partSize: 16 * 1024 * 1024,
  minPartSize: 5 * 1024 * 1024,
  maxPartSize: 64 * 1024 * 1024,
  maxParts: 10_000,
};

const encoder = new TextEncoder();

export function createMockStorage(
  ctx: MockContext,
  options: MockStorageOptions,
  defaults: { buckets: () => Bucket[] },
): MockStorage {
  let counter = 0;
  const nextId = (prefix: string) => {
    counter += 1;
    return `${prefix}${counter.toString(16).padStart(8, '0')}`;
  };

  const buckets = new Map<string, MockBucketState>();
  const usesSamples = options.buckets === undefined;
  const seeds = options.objects ?? (usesSamples ? sampleObjects() : {});
  for (const bucket of options.buckets ?? defaults.buckets()) {
    buckets.set(bucket.name, {
      bucket: clone({ deleteMarkers: 0, public: false, ...bucket }),
      keys: new Map(),
      policy: null,
      rules: [],
      encryption: null,
    });
  }

  function versionIdFor(state: MockBucketState): string {
    return state.bucket.versioning === 'enabled' ? nextId('v-') : 'null';
  }

  /** Adjusts the counters of the bucket by a delta of objects and bytes. */
  function account(state: MockBucketState, objects: number, bytes: number, versions = 0) {
    state.bucket.objects = Math.max(0, state.bucket.objects + objects);
    state.bucket.size = Math.max(0, state.bucket.size + bytes);
    state.bucket.versions = Math.max(0, state.bucket.versions + versions);
  }

  const storage: MockStorage = {
    ctx,
    buckets,
    uploads: new Map(),
    uploadLimits: { ...defaultLimits, ...options.uploadLimits },
    bucket(name) {
      const state = buckets.get(name);
      if (!state) {
        throw problem(404, 'bucket_not_found', `Bucket ${name} does not exist.`);
      }
      return state;
    },
    nextId,
    put(state, version) {
      const list = state.keys.get(version.key) ?? [];
      const latest = list[0];
      const stored: MockVersion = {
        ...version,
        versionId: versionIdFor(state),
        isDeleteMarker: false,
      };
      // Without versioning (or suspended) the "null" version is replaced.
      const kept = list.filter((v) => v.versionId !== stored.versionId);
      const replaced = list.find((v) => v.versionId === stored.versionId);
      state.keys.set(version.key, [stored, ...kept]);
      const wasLive = latest !== undefined && !latest.isDeleteMarker;
      account(
        state,
        wasLive ? 0 : 1,
        stored.size - (replaced && !replaced.isDeleteMarker ? replaced.size : 0),
        replaced ? 0 : 1,
      );
      return stored;
    },
    remove(state, key, versionId) {
      const list = state.keys.get(key);
      if (!list || list.length === 0) {
        return undefined;
      }
      const latest = list[0];
      if (versionId !== undefined && versionId !== '') {
        const target = list.find((v) => v.versionId === versionId);
        if (!target) {
          throw problem(404, 'version_not_found', `Version ${versionId} of ${key} does not exist.`);
        }
        const rest = list.filter((v) => v !== target);
        if (rest.length === 0) {
          state.keys.delete(key);
        } else {
          state.keys.set(key, rest);
        }
        const liveRemoved = target === latest && !target.isDeleteMarker;
        const liveRestored = target === latest && rest[0] !== undefined && !rest[0].isDeleteMarker;
        account(
          state,
          (liveRestored ? 1 : 0) - (liveRemoved ? 1 : 0),
          -(target.isDeleteMarker ? 0 : target.size),
          -1,
        );
        return undefined;
      }
      if (state.bucket.versioning === 'enabled') {
        if (latest?.isDeleteMarker) {
          return undefined;
        }
        const marker: MockVersion = {
          key,
          versionId: nextId('v-'),
          size: 0,
          etag: '',
          lastModified: ctx.nowIso(),
          contentType: '',
          isDeleteMarker: true,
          tags: {},
          userMetadata: {},
        };
        state.keys.set(key, [marker, ...list]);
        state.bucket.deleteMarkers = (state.bucket.deleteMarkers ?? 0) + 1;
        account(state, -1, 0, 1);
        return marker;
      }
      const live = list.filter((v) => !v.isDeleteMarker);
      state.keys.delete(key);
      account(
        state,
        latest && !latest.isDeleteMarker ? -1 : 0,
        -live.reduce((sum, v) => sum + v.size, 0),
        -list.length,
      );
      return undefined;
    },
  };

  for (const [name, objects] of Object.entries(seeds)) {
    const state = buckets.get(name);
    if (!state) {
      continue;
    }
    const before = {
      objects: state.bucket.objects,
      size: state.bucket.size,
      versions: state.bucket.versions,
    };
    for (const seed of objects) {
      const content = seed.content !== undefined ? encoder.encode(seed.content) : undefined;
      const isImage = seed.contentType === 'image/png' && seed.size === tinyPng.byteLength;
      storage.put(state, {
        key: seed.key,
        size: seed.size ?? content?.byteLength ?? 0,
        etag: nextId('e'),
        lastModified: seed.lastModified ?? ctx.nowIso(),
        contentType: seed.contentType ?? 'application/octet-stream',
        tags: { ...seed.tags },
        userMetadata: {},
        ...(content ? { content } : isImage ? { content: tinyPng } : {}),
      });
    }
    // The sample counters already include the sample objects.
    if (usesSamples && options.objects === undefined) {
      Object.assign(state.bucket, before);
    }
  }
  return storage;
}

/** The ObjectEntry of a stored version. */
export function toEntry(version: MockVersion, isLatest: boolean): ObjectEntry {
  return {
    key: version.key,
    ...(version.versionId !== 'null' ? { versionId: version.versionId } : {}),
    size: version.size,
    etag: version.etag,
    lastModified: version.lastModified,
    ...(version.contentType !== '' ? { contentType: version.contentType } : {}),
    isDeleteMarker: version.isDeleteMarker,
    isLatest,
    storageClass: 'STANDARD',
  };
}

/** Bucket policy statements equivalent to the anonymous presets of the rules. */
export function presetPolicy(bucket: string, rules: AccessRule[]): string | null {
  const statements: unknown[] = [];
  for (const rule of rules) {
    const read = rule.preset === 'download' || rule.preset === 'public';
    const write = rule.preset === 'upload' || rule.preset === 'public';
    const actions = [...(read ? ['s3:GetObject'] : []), ...(write ? ['s3:PutObject'] : [])];
    if (actions.length === 0) {
      continue;
    }
    statements.push({
      Effect: 'Allow',
      Principal: { AWS: ['*'] },
      Action: actions,
      Resource: [`arn:aws:s3:::${bucket}/${rule.prefix}*`],
    });
    if (read) {
      statements.push({
        Effect: 'Allow',
        Principal: { AWS: ['*'] },
        Action: ['s3:ListBucket'],
        Resource: [`arn:aws:s3:::${bucket}`],
        Condition: { StringLike: { 's3:prefix': [`${rule.prefix}*`] } },
      });
    }
  }
  if (statements.length === 0) {
    return null;
  }
  return JSON.stringify({ Version: '2012-10-17', Statement: statements }, null, 2);
}

/** Replaces the rule of a prefix (private removes it). */
export function applyPreset(
  rules: AccessRule[],
  prefix: string,
  preset: AccessPreset,
): AccessRule[] {
  const others = rules.filter((r) => r.prefix !== prefix);
  if (preset === 'private') {
    return others;
  }
  return [...others, { prefix, preset }].sort((a, b) => a.prefix.localeCompare(b.prefix));
}
