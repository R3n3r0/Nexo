import type { E2EState } from './env';
import { rootCredentials } from './nexo';
import { type Credentials, encodePath, signRequest } from './sigv4';

export type Query = Record<string, string | number | boolean | undefined>;

export interface CallOptions {
  query?: Query;
  /** JSON body (sent with Content-Type: application/json). */
  json?: unknown;
  /** Raw body. */
  body?: Buffer | string;
  headers?: Record<string, string>;
  /** Sends the request without signature (anonymous). */
  anonymous?: boolean;
}

/** A failed call with the status and the body of the answer. */
export class CallError extends Error {
  constructor(
    readonly status: number,
    readonly body: string,
    message: string,
  ) {
    super(message);
  }
}

/**
 * Signed (SigV4) client of the server under test: the Admin API under
 * /api/v1 of the console listener and the S3 API. The specs use it to
 * prepare fixtures and to check the server state behind the UI.
 */
export class NexoClient {
  constructor(
    private readonly state: Pick<E2EState, 'consoleURL' | 's3URL' | 'region'>,
    private readonly creds: Credentials = rootCredentials,
  ) {}

  /** The same client with other credentials. */
  as(creds: Credentials): NexoClient {
    return new NexoClient(this.state, creds);
  }

  private async send(origin: string, method: string, path: string, options: CallOptions) {
    const url = new URL(origin);
    url.pathname = encodePath(path);
    for (const [key, value] of Object.entries(options.query ?? {})) {
      if (value !== undefined) {
        url.searchParams.set(key, String(value));
      }
    }
    let body = options.body;
    const headers: Record<string, string> = { ...options.headers };
    if (options.json !== undefined) {
      body = JSON.stringify(options.json);
      headers['content-type'] = 'application/json';
    }
    const signed = options.anonymous
      ? headers
      : signRequest({ method, url, headers, body, region: this.state.region }, this.creds);
    const payload = typeof body === 'string' || body === undefined ? body : new Uint8Array(body);
    return fetch(url, { method, headers: signed, body: payload, redirect: 'manual' });
  }

  /** Raw Admin API request (path relative to /api/v1). */
  admin(method: string, path: string, options: CallOptions = {}): Promise<Response> {
    return this.send(this.state.consoleURL, method, `/api/v1${path}`, options);
  }

  /** Admin API request that must succeed; resolves to the decoded JSON (or undefined). */
  async call<T = unknown>(method: string, path: string, options: CallOptions = {}): Promise<T> {
    const res = await this.admin(method, path, options);
    const text = await res.text();
    if (!res.ok) {
      throw new CallError(res.status, text, `${method} ${path}: ${res.status} ${text}`);
    }
    return (text === '' ? undefined : JSON.parse(text)) as T;
  }

  /** Raw S3 request; path is "/bucket" or "/bucket/key" (not encoded). */
  s3(method: string, path: string, options: CallOptions = {}): Promise<Response> {
    return this.send(this.state.s3URL, method, path, options);
  }

  /** Uploads an object with the S3 API. */
  async putObject(bucket: string, key: string, body: Buffer | string, contentType?: string) {
    const res = await this.s3('PUT', `/${bucket}/${key}`, {
      body,
      headers: contentType === undefined ? {} : { 'content-type': contentType },
    });
    if (!res.ok) {
      throw new CallError(res.status, await res.text(), `PUT ${bucket}/${key}: ${res.status}`);
    }
  }

  /** Creates a bucket with the Admin API. */
  async createBucket(name: string, options: { versioning?: boolean } = {}) {
    await this.call('POST', '/buckets', { json: { name, ...options } });
  }

  /** Deletes a bucket and its content, ignoring a missing bucket. */
  async deleteBucket(name: string) {
    const res = await this.admin('DELETE', `/buckets/${name}`, { query: { force: true } });
    if (!res.ok && res.status !== 404) {
      throw new CallError(res.status, await res.text(), `DELETE bucket ${name}: ${res.status}`);
    }
  }

  /** Keys of the objects of a bucket (recursive listing). */
  async listKeys(bucket: string, prefix = ''): Promise<string[]> {
    const keys: string[] = [];
    let cursor: string | undefined;
    do {
      const page = await this.call<{ objects: { key: string }[]; nextCursor?: string }>(
        'GET',
        `/buckets/${bucket}/objects`,
        { query: { prefix, delimiter: '', cursor, limit: 1000 } },
      );
      keys.push(...page.objects.map((o) => o.key));
      cursor = page.nextCursor === '' ? undefined : page.nextCursor;
    } while (cursor !== undefined);
    return keys.sort();
  }
}

/** Unique, DNS-safe name for a bucket or a policy created by a test. */
export function uniqueName(prefix: string): string {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
}

/** Unique access key (3 to 20 characters) for a user created by a test. */
export function uniqueKey(prefix: string): string {
  return `${prefix.slice(0, 10)}-${Math.random().toString(36).slice(2, 10)}`;
}
