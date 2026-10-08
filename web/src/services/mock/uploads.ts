import { ApiError } from '../errors';
import type { UploadService } from '../interfaces';
import { toEntry, type MockStorage } from './storage';
import { problem } from './support';

/** Rejects when the signal is aborted, like the HTTP client does. */
function checkAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted) {
    throw ApiError.aborted(signal.reason);
  }
}

/**
 * Multipart and single uploads in memory. Every transfer reports its full
 * progress once the (artificial) latency elapsed.
 */
export function createMockUploadService(storage: MockStorage): UploadService {
  const { ctx } = storage;

  function upload(bucket: string, key: string, uploadId: string) {
    const state = storage.uploads.get(uploadId);
    if (!state || state.bucket !== bucket || state.key !== key) {
      throw problem(404, 'upload_not_found', `Upload ${uploadId} does not exist.`);
    }
    return state;
  }

  return {
    async putObject(input, options = {}) {
      checkAborted(options.signal);
      await ctx.delay();
      checkAborted(options.signal);
      const state = storage.bucket(input.bucket);
      if (input.key === '' || input.key.startsWith('/')) {
        throw problem(400, 'invalid_object_name', `Object name ${input.key} is not valid.`);
      }
      options.onProgress?.(input.body.size, input.body.size);
      const contentType =
        input.contentType ??
        (input.body.type !== '' ? input.body.type : 'application/octet-stream');
      const stored = storage.put(state, {
        key: input.key,
        size: input.body.size,
        etag: storage.nextId('e'),
        lastModified: ctx.nowIso(),
        contentType,
        tags: {},
        userMetadata: {},
        content: input.body,
      });
      return toEntry(stored, true);
    },
    async create(bucket, key, input = {}) {
      await ctx.delay();
      storage.bucket(bucket);
      const uploadId = storage.nextId('upload-');
      const initiated = ctx.nowIso();
      storage.uploads.set(uploadId, {
        bucket,
        key,
        uploadId,
        initiated,
        contentType: input.contentType ?? 'application/octet-stream',
        userMetadata: { ...input.userMetadata },
        parts: new Map(),
      });
      return { bucket, key, uploadId, initiated, ...storage.uploadLimits };
    },
    async uploadPart(input, options = {}) {
      checkAborted(options.signal);
      await ctx.delay();
      checkAborted(options.signal);
      const state = upload(input.bucket, input.key, input.uploadId);
      if (input.partNumber < 1 || input.partNumber > storage.uploadLimits.maxParts) {
        throw problem(
          400,
          'invalid_argument',
          `Part number ${String(input.partNumber)} is out of range.`,
        );
      }
      if (input.body.size > storage.uploadLimits.maxPartSize) {
        throw problem(413, 'entity_too_large', 'The part is too large.');
      }
      options.onProgress?.(input.body.size, input.body.size);
      const part = {
        etag: storage.nextId('p'),
        size: input.body.size,
        body: input.body,
        lastModified: ctx.nowIso(),
      };
      state.parts.set(input.partNumber, part);
      return {
        partNumber: input.partNumber,
        etag: part.etag,
        size: part.size,
        lastModified: part.lastModified,
      };
    },
    async complete(bucket, key, uploadId, parts) {
      await ctx.delay();
      const state = upload(bucket, key, uploadId);
      if (parts.length === 0) {
        throw problem(400, 'invalid_part', 'At least one part is required.');
      }
      parts.forEach((p, i) => {
        if (i > 0 && p.partNumber <= (parts[i - 1]?.partNumber ?? 0)) {
          throw problem(400, 'invalid_part_order', 'The parts are not in ascending order.');
        }
      });
      const bodies = parts.map((p, i) => {
        const stored = state.parts.get(p.partNumber);
        if (!stored || stored.etag !== p.etag) {
          throw problem(400, 'invalid_part', `Part ${String(p.partNumber)} was not uploaded.`);
        }
        if (i < parts.length - 1 && stored.size < storage.uploadLimits.minPartSize) {
          throw problem(400, 'entity_too_small', `Part ${String(p.partNumber)} is too small.`);
        }
        return stored.body;
      });
      const content = new Blob(bodies, { type: state.contentType });
      const stored = storage.put(storage.bucket(bucket), {
        key,
        size: content.size,
        etag: `${storage.nextId('e')}-${String(parts.length)}`,
        lastModified: ctx.nowIso(),
        contentType: state.contentType,
        tags: {},
        userMetadata: state.userMetadata,
        content,
      });
      storage.uploads.delete(uploadId);
      return toEntry(stored, true);
    },
    async abort(bucket, key, uploadId) {
      await ctx.delay();
      upload(bucket, key, uploadId);
      storage.uploads.delete(uploadId);
    },
    async list(bucket, query = {}) {
      await ctx.delay();
      storage.bucket(bucket);
      const items = [...storage.uploads.values()]
        .filter((u) => u.bucket === bucket && u.key.startsWith(query.prefix ?? ''))
        .sort((a, b) => a.key.localeCompare(b.key))
        .map((u) => ({ key: u.key, uploadId: u.uploadId, initiated: u.initiated }));
      return { items };
    },
  };
}
