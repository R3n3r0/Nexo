import type { UploadService } from '../interfaces';
import type { ObjectEntry, Part, Upload, UploadList } from '../types';
import type { HttpClient } from './client';

/**
 * UploadService over the uploads proxied by the console: the browser never
 * holds S3 credentials, the session authorizes every request.
 */
export function createHttpUploadService(client: HttpClient): UploadService {
  const uploads = (bucket: string, sub = '') =>
    `/buckets/${encodeURIComponent(bucket)}/uploads${sub === '' ? '' : `/${sub}`}`;

  return {
    putObject(input, options = {}) {
      return client.upload<ObjectEntry>(
        `/buckets/${encodeURIComponent(input.bucket)}/objects/upload`,
        {
          method: 'POST',
          query: { key: input.key },
          body: input.body,
          ...(input.contentType !== undefined ? { contentType: input.contentType } : {}),
          ...(options.signal ? { signal: options.signal } : {}),
          ...(options.onProgress ? { onProgress: options.onProgress } : {}),
        },
      );
    },
    create(bucket, key, input = {}) {
      return client.request<Upload>(uploads(bucket), {
        method: 'POST',
        query: { key },
        body: input,
      });
    },
    uploadPart(input, options = {}) {
      return client.upload<Part>(
        uploads(
          input.bucket,
          `${encodeURIComponent(input.uploadId)}/parts/${String(input.partNumber)}`,
        ),
        {
          method: 'PUT',
          query: { key: input.key },
          body: input.body,
          contentType: 'application/octet-stream',
          ...(options.signal ? { signal: options.signal } : {}),
          ...(options.onProgress ? { onProgress: options.onProgress } : {}),
        },
      );
    },
    complete(bucket, key, uploadId, parts) {
      return client.request<ObjectEntry>(
        uploads(bucket, `${encodeURIComponent(uploadId)}/complete`),
        { method: 'POST', query: { key }, body: { parts } },
      );
    },
    abort(bucket, key, uploadId) {
      return client.request<void>(uploads(bucket, encodeURIComponent(uploadId)), {
        method: 'DELETE',
        query: { key },
      });
    },
    list(bucket, query = {}) {
      return client.request<UploadList>(uploads(bucket), {
        query: { prefix: query.prefix, cursor: query.cursor, limit: query.limit },
      });
    },
  };
}
