import { ApiError, ClientErrorCode, type ProblemDetails } from '../errors';
import { defaultCsrfCookieName, defaultCsrfHeaderName, readDocumentCookie } from './csrf';

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

export interface HttpClientOptions {
  /** Base path or URL of the Admin API. Defaults to "/api/v1". */
  baseUrl?: string;
  /** fetch implementation; defaults to the global one. */
  fetch?: typeof fetch;
  csrfCookieName?: string;
  csrfHeaderName?: string;
  /** Reads a cookie by name; defaults to document.cookie. */
  readCookie?: (name: string) => string | undefined;
  /** Called on every 401 response unless the request opted out. */
  onUnauthorized?: () => void;
}

/**
 * Minimal JSON client for the Admin API. It resolves paths against the base
 * URL, decodes problem+json failures into ApiError, adds the CSRF header on
 * mutating requests and raises the unauthorized event on 401.
 */
export interface HttpClient {
  request<T>(path: string, options?: RequestOptions): Promise<T>;
  /**
   * Sets the CSRF token to use when the cookie is not readable (for example
   * when the server marks it HttpOnly and returns it in the login payload).
   */
  setCsrfToken(token: string | undefined): void;
}

const mutatingMethods: ReadonlySet<HttpMethod> = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

export function createHttpClient(options: HttpClientOptions = {}): HttpClient {
  const baseUrl = options.baseUrl ?? '/api/v1';
  const csrfCookieName = options.csrfCookieName ?? defaultCsrfCookieName;
  const csrfHeaderName = options.csrfHeaderName ?? defaultCsrfHeaderName;
  const readCookie = options.readCookie ?? readDocumentCookie;
  // Bind lazily so tests and MSW can replace the global fetch after creation.
  const doFetch: typeof fetch = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  let memoryToken: string | undefined;

  async function request<T>(path: string, requestOptions: RequestOptions = {}): Promise<T> {
    const method = requestOptions.method ?? 'GET';
    const headers = new Headers({ Accept: 'application/json, application/problem+json' });
    const init: RequestInit = { method, headers, credentials: 'same-origin' };
    if (requestOptions.signal) {
      init.signal = requestOptions.signal;
    }
    if (requestOptions.body !== undefined) {
      headers.set('Content-Type', 'application/json');
      init.body = JSON.stringify(requestOptions.body);
    }
    if (mutatingMethods.has(method)) {
      const token = readCookie(csrfCookieName) ?? memoryToken;
      if (token !== undefined && token !== '') {
        headers.set(csrfHeaderName, token);
      }
    }

    let response: Response;
    try {
      response = await doFetch(resolveUrl(baseUrl, path, requestOptions.query), init);
    } catch (cause) {
      throw ApiError.network(cause);
    }

    if (!response.ok) {
      const error = await decodeError(response);
      if (error.isUnauthorized && requestOptions.notifyUnauthorized !== false) {
        options.onUnauthorized?.();
      }
      throw error;
    }
    return decodeBody<T>(response);
  }

  return {
    request,
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
