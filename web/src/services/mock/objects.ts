import { saveBlob } from '@/lib/download';
import type { ObjectService } from '../interfaces';
import type { DeleteObjectsResult, ObjectDetails, ObjectList, RenameResult } from '../types';
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

  function deleteKey(
    state: MockBucketState,
    key: string,
    versionId: string | undefined,
    result: DeleteObjectsResult,
  ) {
    try {
      const marker = storage.remove(state, key, versionId);
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
        code: 'internal_error',
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
      for (const ref of input.objects ?? []) {
        deleteKey(state, ref.key, ref.versionId, result);
      }
      for (const prefix of input.prefixes ?? []) {
        for (const key of [...state.keys.keys()].filter((k) => k.startsWith(prefix)).sort()) {
          if (input.allVersions === true) {
            for (const v of [...(state.keys.get(key) ?? [])]) {
              deleteKey(state, key, v.versionId, result);
            }
          } else {
            deleteKey(state, key, undefined, result);
          }
        }
      }
      return result;
    },
    async rename(bucket, input) {
      await ctx.delay();
      const state = storage.bucket(bucket);
      const sources =
        input.prefix === true
          ? liveKeys(state).filter((k) => k.startsWith(input.source))
          : [latestLive(state, input.source).key];
      if (input.prefix === true && input.destination.startsWith(input.source)) {
        throw problem(400, 'invalid_argument', 'The destination lies under the source.');
      }
      const result: RenameResult = { renamed: [], errors: [] };
      for (const source of sources) {
        const destination =
          input.prefix === true
            ? input.destination + source.slice(input.source.length)
            : input.destination;
        const exists = state.keys.get(destination)?.[0];
        if (exists && !exists.isDeleteMarker && input.overwrite !== true) {
          if (input.prefix !== true) {
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
        const from = latestLive(state, source);
        storage.put(state, { ...from, key: destination, lastModified: ctx.nowIso() });
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
        ...(input.prefixes ?? []).flatMap((p) => liveKeys(state).filter((k) => k.startsWith(p))),
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
