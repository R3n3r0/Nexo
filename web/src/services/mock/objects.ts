import { saveBlob } from '@/lib/download';
import { isApiError } from '../errors';
import type { ObjectService } from '../interfaces';
import type {
  DeleteObjectsResult,
  ObjectDetails,
  ObjectList,
  ObjectRetention,
  RenameResult,
} from '../types';
import { toEntry, type MockBucketState, type MockStorage, type MockVersion } from './storage';
import { clone, problem } from './support';

/** Page size of the mock listings (the server default). */
const defaultLimit = 1000;

/** Keys whose latest version is live (not a delete marker), sorted. */
function liveKeys(state: MockBucketState): string[] {
  const keys: string[] = [];
  for (const [key, versions] of state.keys) {
    if (versions[0] && !versions[0].isDeleteMarker) {
      keys.push(key);
    }
  }
  return keys.sort();
}

/**
 * A prefix of the bulk operations as the folder it names, like the server
 * (admin.folderPrefix): "photos" is "photos/", never "photos2/"; "" is the
 * whole bucket.
 */
function folderPrefix(prefix: string): string {
  return prefix === '' || prefix.endsWith('/') ? prefix : `${prefix}/`;
}

function latestLive(state: MockBucketState, key: string): MockVersion {
  const latest = state.keys.get(key)?.[0];
  if (!latest || latest.isDeleteMarker) {
    throw problem(404, 'object_not_found', `Object ${key} does not exist.`);
  }
  return latest;
}

function findVersion(state: MockBucketState, key: string, versionId?: string): MockVersion {
  if (versionId === undefined || versionId === '') {
    return latestLive(state, key);
  }
  const version = state.keys.get(key)?.find((v) => v.versionId === versionId);
  if (!version) {
    throw problem(404, 'version_not_found', `Version ${versionId} of ${key} does not exist.`);
  }
  return version;
}

/** The cursor of a mock listing: base64 of the UTF-8 bytes of the last name. */
function encodeCursor(name: string): string {
  return btoa(String.fromCharCode(...new TextEncoder().encode(name)));
}

function decodeCursor(cursor: string): string {
  return new TextDecoder().decode(Uint8Array.from(atob(cursor), (c) => c.charCodeAt(0)));
}

/** Base64 data URL of small in-memory contents (demo previews and downloads). */
function dataUrl(version: MockVersion): string {
  const content = version.content;
  if (!ArrayBuffer.isView(content) || content.byteLength > 512 * 1024) {
    return '#';
  }
  let binary = '';
  for (const byte of content) {
    binary += String.fromCharCode(byte);
  }
  return `data:${version.contentType || 'application/octet-stream'};base64,${btoa(binary)}`;
}

async function readText(version: MockVersion, maxBytes: number) {
  const content = version.content;
  if (content === undefined) {
    return { text: '', bytes: 0 };
  }
  if (ArrayBuffer.isView(content)) {
    const head = content.subarray(0, maxBytes);
    return { text: new TextDecoder().decode(head), bytes: head.byteLength };
  }
  const head = content.slice(0, maxBytes);
  const text = typeof head.text === 'function' ? await head.text() : '';
  return { text, bytes: head.size };
}

export function createMockObjectService(storage: MockStorage): ObjectService {
  const { ctx } = storage;

  /** The bucket with object lock, or 400 invalid_request like the server. */
  function lockedBucket(bucket: string): MockBucketState {
    const state = storage.bucket(bucket);
    if (!state.bucket.objectLockEnabled) {
      throw problem(400, 'invalid_request', 'Bucket is missing Object Lock Configuration.');
    }
    return state;
  }

  /** The version addressed by an Object Lock operation (never a delete marker). */
  function lockTarget(state: MockBucketState, key: string, versionId?: string): MockVersion {
    const version = findVersion(state, key, versionId);
    if (version.isDeleteMarker) {
      throw problem(405, 'method_not_allowed', 'The version is a delete marker.');
    }
    return version;
  }

  function retentionOf(version: MockVersion): ObjectRetention {
    return {
      ...(version.versionId !== 'null' ? { versionId: version.versionId } : {}),
      ...(version.retention
        ? { mode: version.retention.mode, retainUntil: version.retention.retainUntil }
        : {}),
    };
  }

  function deleteKey(
    state: MockBucketState,
    key: string,
    versionId: string | undefined,
    result: DeleteObjectsResult,
    bypassGovernance: boolean,
  ) {
    try {
      const marker = storage.remove(state, key, versionId, { bypassGovernance });
      result.deleted.push({
        key,
        ...(versionId ? { versionId } : {}),
        deleteMarker: marker !== undefined,
        ...(marker ? { deleteMarkerVersionId: marker.versionId } : {}),
      });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      result.errors.push({
        key,
        ...(versionId ? { versionId } : {}),
        code: isApiError(error) ? error.code : 'internal_error',
        message,
      });
    }
  }

  return {
    async list(bucket, query = {}) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const prefix = query.prefix ?? '';
      const delimiter = query.delimiter ?? '/';
      const limit = Math.min(Math.max(query.limit ?? defaultLimit, 1), defaultLimit);
      const after =
        query.cursor !== undefined && query.cursor !== '' ? decodeCursor(query.cursor) : '';

      // Build the merged, sorted sequence of keys and common prefixes.
      const entries: { name: string; prefix: boolean }[] = [];
      const seen = new Set<string>();
      for (const key of liveKeys(state)) {
        if (!key.startsWith(prefix) || key === prefix) {
          continue;
        }
        const rest = key.slice(prefix.length);
        const cut = delimiter === '' ? -1 : rest.indexOf(delimiter);
        if (cut >= 0) {
          const common = prefix + rest.slice(0, cut + delimiter.length);
          if (!seen.has(common)) {
            seen.add(common);
            entries.push({ name: common, prefix: true });
          }
        } else {
          entries.push({ name: key, prefix: false });
        }
      }
      entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
      const start = after === '' ? 0 : entries.findIndex((e) => e.name > after);
      const page = start < 0 ? [] : entries.slice(start, start + limit);
      const truncated = start >= 0 && start + limit < entries.length;
      const last = page[page.length - 1];
      const out: ObjectList = {
        objects: page.filter((e) => !e.prefix).map((e) => toEntry(latestLive(state, e.name), true)),
        prefixes: page.filter((e) => e.prefix).map((e) => e.name),
        isTruncated: truncated,
        ...(truncated && last ? { nextCursor: encodeCursor(last.name) } : {}),
      };
      return out;
    },
    async stat(bucket, key, versionId) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const version = findVersion(state, key, versionId);
      if (version.isDeleteMarker) {
        throw problem(405, 'method_not_allowed', `Version ${versionId ?? ''} is a delete marker.`);
      }
      const all = state.keys.get(key) ?? [];
      const details: ObjectDetails = {
        ...toEntry(version, all[0] === version),
        userMetadata: { ...version.userMetadata },
        ...(version.parts !== undefined ? { parts: version.parts.length } : {}),
        ...(version.checksum
          ? { checksumAlgorithm: version.checksum.algorithm, checksum: version.checksum.value }
          : {}),
        ...(version.encryption ? { encryption: clone(version.encryption) } : {}),
        tags: { ...version.tags },
        versions: all.slice(0, 10).map((v, i) => toEntry(v, i === 0)),
        versionsTruncated: all.length > 10,
      };
      return details;
    },
    async versions(bucket, key, query = {}) {
      await ctx.delay();
      const all = storage.bucket(bucket).keys.get(key) ?? [];
      const limit = Math.min(Math.max(query.limit ?? defaultLimit, 1), defaultLimit);
      const start = query.cursor !== undefined && query.cursor !== '' ? Number(query.cursor) : 0;
      const items = all.slice(start, start + limit).map((v) => toEntry(v, v === all[0]));
      const next = start + limit < all.length ? String(start + limit) : undefined;
      return { items, ...(next !== undefined ? { nextCursor: next } : {}) };
    },
    async createFolder(bucket, prefix) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const key = prefix.endsWith('/') ? prefix : `${prefix}/`;
      if (key === '/' || key.startsWith('/')) {
        throw problem(400, 'invalid_object_name', `Folder name ${prefix} is not valid.`);
      }
      const stored = storage.put(state, {
        key,
        size: 0,
        etag: storage.nextId('e'),
        lastModified: ctx.nowIso(),
        contentType: 'application/x-directory',
        tags: {},
        userMetadata: {},
      });
      return toEntry(stored, true);
    },
    async remove(bucket, input) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const result: DeleteObjectsResult = { deleted: [], errors: [] };
      const bypass = input.bypassGovernance === true;
      for (const ref of input.objects ?? []) {
        deleteKey(state, ref.key, ref.versionId, result, bypass);
      }
      for (const prefix of (input.prefixes ?? []).map(folderPrefix)) {
        for (const key of [...state.keys.keys()].filter((k) => k.startsWith(prefix)).sort()) {
          if (input.allVersions === true) {
            for (const v of [...(state.keys.get(key) ?? [])]) {
              deleteKey(state, key, v.versionId, result, bypass);
            }
          } else {
            deleteKey(state, key, undefined, result, bypass);
          }
        }
      }
      return result;
    },
    async restoreVersion(bucket, key, versionId) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const version = findVersion(state, key, versionId);
      if (version.isDeleteMarker) {
        throw problem(405, 'method_not_allowed', 'A delete marker cannot be restored.');
      }
      const restored = storage.put(state, {
        key,
        size: version.size,
        etag: version.etag,
        lastModified: ctx.now().toISOString(),
        contentType: version.contentType,
        tags: { ...version.tags },
        userMetadata: { ...version.userMetadata },
        ...(version.content !== undefined ? { content: version.content } : {}),
      });
      return toEntry(restored, true);
    },
    async rename(bucket, input) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const folders = input.prefix === true;
      const from = folders ? folderPrefix(input.source) : input.source;
      const to = folders ? folderPrefix(input.destination) : input.destination;
      const sources = folders
        ? liveKeys(state).filter((k) => k.startsWith(from))
        : [latestLive(state, from).key];
      if (folders && to.startsWith(from)) {
        throw problem(400, 'invalid_argument', 'The destination lies under the source.');
      }
      const result: RenameResult = { renamed: [], errors: [] };
      for (const source of sources) {
        const destination = folders ? to + source.slice(from.length) : to;
        const exists = state.keys.get(destination)?.[0];
        if (exists && !exists.isDeleteMarker && input.overwrite !== true) {
          if (!folders) {
            throw problem(409, 'conflict', `Object ${destination} already exists.`);
          }
          result.errors.push({
            source,
            destination,
            code: 'conflict',
            message: 'The destination exists.',
          });
          continue;
        }
        const latest = latestLive(state, source);
        storage.put(state, { ...latest, key: destination, lastModified: ctx.nowIso() });
        storage.remove(state, source);
        result.renamed.push({ source, destination });
      }
      return result;
    },
    async setTags(bucket, key, tags, versionId) {
      await ctx.delay();
      const version = findVersion(storage.bucket(bucket), key, versionId);
      version.tags = { ...tags };
      return clone(version.tags);
    },
    async getRetention(bucket, key, versionId) {
      await ctx.delay();
      return retentionOf(lockTarget(lockedBucket(bucket), key, versionId));
    },
    async setRetention(bucket, key, input, versionId) {
      await ctx.delay();
      const version = lockTarget(lockedBucket(bucket), key, versionId);
      const mode = input.mode?.toUpperCase() ?? '';
      const until = input.retainUntil ?? '';
      const now = ctx.now().getTime();
      const current = version.retention;
      const active = current !== undefined && Date.parse(current.retainUntil) > now;
      if (mode === '' && until === '') {
        if (active) {
          if (current.mode === 'COMPLIANCE') {
            throw problem(403, 'object_locked', 'A COMPLIANCE retention cannot be removed.');
          }
          if (input.bypassGovernance !== true) {
            throw problem(
              403,
              'object_locked',
              'Removing a GOVERNANCE retention needs the governance bypass.',
            );
          }
        }
        delete version.retention;
        return retentionOf(version);
      }
      if (mode !== 'GOVERNANCE' && mode !== 'COMPLIANCE') {
        throw problem(400, 'malformed_xml', 'The retention mode must be GOVERNANCE or COMPLIANCE.');
      }
      const target = Date.parse(until);
      if (Number.isNaN(target) || target <= now) {
        throw problem(400, 'invalid_argument', 'The retain until date must be in the future.');
      }
      if (active) {
        const shorter = target < Date.parse(current.retainUntil);
        const weaker = current.mode === 'COMPLIANCE' && mode === 'GOVERNANCE';
        if (current.mode === 'COMPLIANCE' && (shorter || weaker)) {
          throw problem(403, 'object_locked', 'A COMPLIANCE retention cannot be shortened.');
        }
        if (current.mode === 'GOVERNANCE' && shorter && input.bypassGovernance !== true) {
          throw problem(
            403,
            'object_locked',
            'Shortening a GOVERNANCE retention needs the governance bypass.',
          );
        }
      }
      version.retention = { mode, retainUntil: new Date(target).toISOString() };
      return retentionOf(version);
    },
    async getLegalHold(bucket, key, versionId) {
      await ctx.delay();
      return lockTarget(lockedBucket(bucket), key, versionId).legalHold ?? 'OFF';
    },
    async setLegalHold(bucket, key, status, versionId) {
      await ctx.delay();
      const version = lockTarget(lockedBucket(bucket), key, versionId);
      version.legalHold = status;
      return status;
    },
    async attributes(bucket, key, query = {}) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const version = lockTarget(state, key, query.versionId);
      const all = version.parts ?? [];
      const marker = query.partNumberMarker ?? 0;
      const max = Math.min(Math.max(query.maxParts ?? 1000, 1), 1000);
      const after = all.filter((p) => p.partNumber > marker);
      const page = after.slice(0, max);
      const truncated = after.length > page.length;
      return {
        key: version.key,
        ...(version.versionId !== 'null' ? { versionId: version.versionId } : {}),
        etag: version.etag,
        size: version.size,
        lastModified: version.lastModified,
        storageClass: 'STANDARD',
        ...(version.checksum ? { checksum: clone(version.checksum) } : {}),
        partsCount: all.length,
        parts: clone(page),
        isTruncated: truncated,
        ...(truncated ? { nextPartNumberMarker: page[page.length - 1]?.partNumber ?? marker } : {}),
      };
    },
    downloadUrl(bucket, key, versionId) {
      const version = storage.buckets
        .get(bucket)
        ?.keys.get(key)
        ?.find((v) => (versionId ? v.versionId === versionId : !v.isDeleteMarker));
      return version ? dataUrl(version) : '#';
    },
    previewUrl(bucket, key, versionId) {
      const version = storage.buckets
        .get(bucket)
        ?.keys.get(key)
        ?.find((v) => (versionId ? v.versionId === versionId : !v.isDeleteMarker));
      return version ? dataUrl(version) : '#';
    },
    async previewText(bucket, key, { maxBytes, versionId, signal }) {
      await ctx.delay();
      if (signal?.aborted) {
        throw problem(0, 'aborted', 'The request was cancelled.');
      }
      const version = findVersion(storage.bucket(bucket), key, versionId);
      const { text, bytes } = await readText(version, maxBytes);
      return {
        text,
        truncated: version.size > bytes,
        size: version.size,
        contentType: version.contentType,
      };
    },
    async downloadZip(bucket, input) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      for (const key of input.keys ?? []) {
        latestLive(state, key);
      }
      const names = [
        ...(input.keys ?? []),
        ...(input.prefixes ?? [])
          .map(folderPrefix)
          .flatMap((p) => liveKeys(state).filter((k) => k.startsWith(p))),
      ];
      // The demo mode saves a listing in place of the archive; the tests
      // (no object URLs in jsdom) only check the request.
      if (typeof URL.createObjectURL === 'function') {
        saveBlob(
          new Blob([`mock zip of ${String(names.length)} objects\n${names.join('\n')}`], {
            type: 'text/plain',
          }),
          `${bucket}-mock-zip.txt`,
        );
      }
    },
  };
}
