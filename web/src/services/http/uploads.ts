import type { UploadService } from '../interfaces';
import type { ObjectEntry, Part, Upload, UploadEncryption, UploadList } from '../types';
import type { HttpClient } from './client';

/**
 * The S3 headers of the server-side encryption of an upload
 * (x-amz-server-side-encryption[-aws-kms-key-id]); none without one.
 */
export function encryptionHeaders(encryption?: UploadEncryption): Record<string, string> {
  if (encryption === undefined) {
    return {};
  }
  const headers: Record<string, string> = {
    'X-Amz-Server-Side-Encryption': encryption.algorithm,
  };
  const keyId = encryption.kmsKeyId?.trim() ?? '';
  if (encryption.algorithm === 'aws:kms' && keyId !== '') {
    headers['X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id'] = keyId;
  }
  return headers;
}

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
          headers: encryptionHeaders(input.encryption),
          ...(options.signal ? { signal: options.signal } : {}),
          ...(options.onProgress ? { onProgress: options.onProgress } : {}),
        },
      );
    },
    create(bucket, key, input = {}) {
      const { encryption, ...body } = input;
      return client.request<Upload>(uploads(bucket), {
        method: 'POST',
        query: { key },
        headers: encryptionHeaders(encryption),
        body,
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
