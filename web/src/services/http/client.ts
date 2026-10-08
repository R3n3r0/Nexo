import { ApiError, ClientErrorCode, type ProblemDetails } from '../errors';
import { defaultCsrfHeaderName } from './csrf';
import { type FormDownloadOptions, submitFormDownload } from './formDownload';

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';

export type QueryParams = Record<string, string | number | boolean | undefined>;

export interface RequestOptions {
  method?: HttpMethod;
  /** JSON-serialised request body. */
  body?: unknown;
  query?: QueryParams;
  signal?: AbortSignal;
  /**
   * When false, a 401 response does not raise the global unauthorized event.
   * Used by the login call, whose 401 means "wrong credentials", not "session expired".
   */
  notifyUnauthorized?: boolean;
}

/** A request whose successful answer is not JSON (downloads, text previews). */
export interface RawRequestOptions {
  method?: HttpMethod;
  query?: QueryParams;
  headers?: Record<string, string>;
  /** JSON-serialised request body. */
  body?: unknown;
  /** Body sent verbatim (set its Content-Type in headers); wins over body. */
  rawBody?: string | Blob;
  signal?: AbortSignal;
}

/** A request with a binary body whose upload progress is reported. */
export interface UploadRequestOptions {
  method?: 'POST' | 'PUT';
  query?: QueryParams;
  body: Blob;
  /** Content-Type of the body; defaults to the type of the blob. */
  contentType?: string;
  signal?: AbortSignal;
  /** Bytes sent so far out of total. */
  onProgress?: (loaded: number, total: number) => void;
}

export interface HttpClientOptions {
  /** Base path or URL of the Admin API. Defaults to "/api/v1". */
  baseUrl?: string;
  /** fetch implementation; defaults to the global one. */
  fetch?: typeof fetch;
  csrfHeaderName?: string;
  /** Called on every 401 response unless the request opted out. */
  onUnauthorized?: () => void;
  /** XMLHttpRequest factory used by upload(); defaults to the global one. */
  xhr?: () => XMLHttpRequest;
}

/**
 * Minimal JSON client for the Admin API. It resolves paths against the base
 * URL, decodes problem+json failures into ApiError, adds the CSRF header on
 * mutating requests and raises the unauthorized event on 401.
 *
 * The CSRF token is held in memory only (setCsrfToken, from the login and
 * session payloads): the server never puts it in a cookie, which the pages
 * of the other ports of the host (the S3 listener) could read.
 */
export interface HttpClient {
  request<T>(path: string, options?: RequestOptions): Promise<T>;
  /**
   * Absolute URL of an API path, for the links the browser follows itself
   * with the session cookie (downloads, previews).
   */
  url(path: string, query?: QueryParams): string;
  /** Like request, but resolves to the raw successful Response. */
  send(path: string, options?: RawRequestOptions): Promise<Response>;
  /**
   * Sends a binary body with XMLHttpRequest, the only browser API that
   * reports the upload progress, and decodes the JSON answer.
   */
  upload<T>(path: string, options: UploadRequestOptions): Promise<T>;
  /**
   * Submits a POST form (fields, plus the CSRF token) as the navigation of
   * a hidden frame, so that the browser streams the attachment it answers
   * to disk. Resolves once the attachment starts; rejects with the problem
   * of a refused request.
   */
  download(
    path: string,
    fields: Record<string, string>,
    options?: FormDownloadOptions,
  ): Promise<void>;
  /**
   * Sets the CSRF token of the session (from the login and session
   * payloads) sent on mutating requests; undefined forgets it.
   */
  setCsrfToken(token: string | undefined): void;
}

const mutatingMethods: ReadonlySet<HttpMethod> = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

export function createHttpClient(options: HttpClientOptions = {}): HttpClient {
  const baseUrl = options.baseUrl ?? '/api/v1';
  const csrfHeaderName = options.csrfHeaderName ?? defaultCsrfHeaderName;
  // Bind lazily so tests and MSW can replace the global fetch after creation.
  const doFetch: typeof fetch = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  let memoryToken: string | undefined;

  function csrfHeader(method: HttpMethod, headers: Record<string, string>): void {
    if (mutatingMethods.has(method)) {
      if (memoryToken !== undefined && memoryToken !== '') {
        headers[csrfHeaderName] = memoryToken;
      }
    }
  }

  async function fail(response: Response, notifyUnauthorized: boolean | undefined): Promise<never> {
    const error = await decodeError(response);
    if (error.isUnauthorized && notifyUnauthorized !== false) {
      options.onUnauthorized?.();
    }
    throw error;
  }

  async function perform(path: string, requestOptions: RequestOptions & RawRequestOptions) {
    const method = requestOptions.method ?? 'GET';
    // A plain record (not Headers): what the caller sets reaches fetch as is.
    const headers: Record<string, string> = {
      Accept: 'application/json, application/problem+json',
      ...requestOptions.headers,
    };
    const init: RequestInit = { method, headers, credentials: 'same-origin' };
    if (requestOptions.signal) {
      init.signal = requestOptions.signal;
    }
    if (requestOptions.rawBody !== undefined) {
      init.body = requestOptions.rawBody;
    } else if (requestOptions.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(requestOptions.body);
    }
    csrfHeader(method, headers);

    let response: Response;
    try {
      response = await doFetch(resolveUrl(baseUrl, path, requestOptions.query), init);
    } catch (cause) {
      if (isAbort(cause, requestOptions.signal)) {
        throw ApiError.aborted(cause);
      }
      throw ApiError.network(cause);
    }

    if (!response.ok) {
      return fail(response, requestOptions.notifyUnauthorized);
    }
    return response;
  }

  async function request<T>(path: string, requestOptions: RequestOptions = {}): Promise<T> {
    const response = await perform(path, requestOptions);
    return decodeBody<T>(response);
  }

  function upload<T>(path: string, uploadOptions: UploadRequestOptions): Promise<T> {
    const method = uploadOptions.method ?? 'PUT';
    const { signal, onProgress, body } = uploadOptions;
    return new Promise<T>((resolve, reject) => {
      if (signal?.aborted) {
        reject(ApiError.aborted(signal.reason));
        return;
      }
      const xhr = options.xhr ? options.xhr() : new XMLHttpRequest();
      const onAbort = () => {
        xhr.abort();
      };
      const settle = () => {
        signal?.removeEventListener('abort', onAbort);
      };
      xhr.open(method, resolveUrl(baseUrl, path, uploadOptions.query));
      xhr.withCredentials = true;
      xhr.setRequestHeader('Accept', 'application/json, application/problem+json');
      const contentType = uploadOptions.contentType ?? body.type;
      xhr.setRequestHeader(
        'Content-Type',
        contentType !== '' ? contentType : 'application/octet-stream',
      );
      const headers: Record<string, string> = {};
      csrfHeader(method, headers);
      for (const [name, value] of Object.entries(headers)) {
        xhr.setRequestHeader(name, value);
      }
      if (onProgress) {
        xhr.upload.onprogress = (event) => {
          onProgress(event.loaded, event.lengthComputable ? event.total : body.size);
        };
      }
      xhr.onload = () => {
        settle();
        const response = new Response(xhr.status === 204 ? null : xhr.responseText, {
          status: xhr.status,
          statusText: xhr.statusText,
          headers: { 'content-type': xhr.getResponseHeader('content-type') ?? '' },
        });
        if (!response.ok) {
          fail(response, true).catch(reject);
          return;
        }
        onProgress?.(body.size, body.size);
        decodeBody<T>(response).then(resolve, reject);
      };
      xhr.onerror = () => {
        settle();
        reject(ApiError.network(new Error('The upload failed.')));
      };
      xhr.onabort = () => {
        settle();
        reject(ApiError.aborted(signal?.reason));
      };
      signal?.addEventListener('abort', onAbort);
      xhr.send(body);
    });
  }

  return {
    request,
    url(path, query) {
      return resolveUrl(baseUrl, path, query);
    },
    send(path, sendOptions = {}) {
      return perform(path, sendOptions);
    },
    upload,
    download(path, fields, downloadOptions) {
      return submitFormDownload(
        {
          csrfToken: () => memoryToken,
          ...(options.onUnauthorized ? { onUnauthorized: options.onUnauthorized } : {}),
        },
        resolveUrl(baseUrl, path),
        fields,
        downloadOptions,
      );
    },
    setCsrfToken(token) {
      memoryToken = token;
    },
  };
}

/**
 * Joins the base URL and the path and appends the defined query parameters.
 * Relative bases ("/api/v1") resolve against the current document location.
 */
export function resolveUrl(baseUrl: string, path: string, query?: QueryParams): string {
  const origin =
    typeof window !== 'undefined' && window.location ? window.location.href : 'http://localhost/';
  const joined = `${baseUrl.replace(/\/+$/, '')}/${path.replace(/^\/+/, '')}`;
  const url = new URL(joined, origin);
  if (query) {
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined) {
        url.searchParams.set(key, String(value));
      }
    }
  }
  return url.toString();
}

/** True when a fetch failure is the cancellation of the request. */
function isAbort(cause: unknown, signal: AbortSignal | undefined): boolean {
  if (signal?.aborted) {
    return true;
  }
  return cause instanceof Error && cause.name === 'AbortError';
}

async function decodeError(response: Response): Promise<ApiError> {
  const contentType = response.headers.get('content-type') ?? '';
  if (contentType.includes('json')) {
    try {
      const body: unknown = await response.json();
      if (isProblemLike(body)) {
        return ApiError.fromProblem(body, response.status);
      }
    } catch {
      // Fall through to the status-only error.
    }
  }
  return ApiError.fromStatus(response.status, response.statusText);
}

async function decodeBody<T>(response: Response): Promise<T> {
  if (response.status === 204 || response.headers.get('content-length') === '0') {
    return undefined as T;
  }
  const text = await response.text();
  if (text === '') {
    return undefined as T;
  }
  try {
    return JSON.parse(text) as T;
  } catch (cause) {
    throw new ApiError(
      {
        title: 'Invalid response',
        status: 0,
        detail: 'The server returned a response that is not valid JSON.',
        code: ClientErrorCode.InvalidResponse,
      },
      { cause },
    );
  }
}

function isProblemLike(value: unknown): value is Partial<ProblemDetails> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
