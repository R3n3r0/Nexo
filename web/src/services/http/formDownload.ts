import { ApiError, ClientErrorCode } from '../errors';
import { readCookie } from './csrf';

/** Form field the Admin API reads the CSRF token from (session.CSRFFormField). */
export const csrfFormField = 'csrfToken';

/** Form field of the token that names the download cookie (admin.DownloadTokenField). */
export const downloadTokenField = 'downloadToken';

/** Prefix of the cookie set when the attachment starts (admin.DownloadCookiePrefix). */
export const downloadCookiePrefix = 'nexo_dl_';

/** Default wait for the server to start the attachment. */
const defaultTimeoutMs = 10 * 60 * 1000;

/** Interval of the checks of the download cookie. */
const pollMs = 200;

/** Delay before the frame of a started download is removed. */
const cleanupMs = 60 * 1000;

export interface FormDownloadOptions {
  signal?: AbortSignal;
  /** Milliseconds to wait for the server to start the attachment. */
  timeoutMs?: number;
}

export interface FormDownloadDeps {
  /** The CSRF token of the session, when known. */
  csrfToken: () => string | undefined;
  /** Called when the server answers 401. */
  onUnauthorized?: () => void;
}

/** A random token of 32 hexadecimal digits. */
function randomToken(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * True for the initial empty about:blank document of the frame: some
 * browsers fire its load event late, after the form was submitted.
 */
function initialDocument(frame: HTMLIFrameElement): boolean {
  try {
    const href = frame.contentWindow?.location.href ?? '';
    const text = frame.contentDocument?.body?.textContent ?? '';
    return href === 'about:blank' && text === '';
  } catch {
    // A cross-origin document is an answer (of a proxy, for instance).
    return false;
  }
}

/** The problem document a refused download loaded in the frame. */
function frameError(frame: HTMLIFrameElement): ApiError {
  let text = '';
  try {
    text = frame.contentDocument?.body?.textContent ?? '';
  } catch {
    // An unreadable frame: no details.
  }
  try {
    const body: unknown = JSON.parse(text);
    if (typeof body === 'object' && body !== null && !Array.isArray(body)) {
      return ApiError.fromProblem(body, 0);
    }
  } catch {
    // Not a problem document.
  }
  return new ApiError({
    title: 'Download failed',
    status: 0,
    detail: 'The server refused the download.',
    code: ClientErrorCode.Http,
  });
}

/**
 * Submits a POST form to url as the navigation of a hidden frame, with
 * the CSRF token as a field: the browser streams an attachment answer to
 * disk instead of holding it in memory. It resolves once the server starts
 * the attachment (the answer sets the cookie named by the download token:
 * a download never loads the frame) and rejects with the problem of a
 * refused request (which loads in the frame), on abort and on timeout.
 */
export function submitFormDownload(
  deps: FormDownloadDeps,
  url: string,
  fields: Record<string, string>,
  options: FormDownloadOptions = {},
): Promise<void> {
  const { signal } = options;
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(ApiError.aborted(signal.reason));
      return;
    }
    const token = randomToken();
    const cookieName = `${downloadCookiePrefix}${token}`;
    const frame = document.createElement('iframe');
    frame.name = `nexo-download-${token}`;
    frame.title = 'download';
    frame.hidden = true;
    frame.tabIndex = -1;
    frame.setAttribute('aria-hidden', 'true');
    document.body.appendChild(frame);

    // The handlers run after the timers below are set.
    const settle = (removeFrame: boolean) => {
      clearInterval(poll);
      clearTimeout(timer);
      frame.removeEventListener('load', onLoad);
      signal?.removeEventListener('abort', onAbort);
      if (removeFrame) {
        frame.remove();
      }
    };
    const onLoad = () => {
      if (initialDocument(frame)) {
        return;
      }
      // Read the problem before the frame goes with its document.
      const error = frameError(frame);
      settle(true);
      if (error.isUnauthorized) {
        deps.onUnauthorized?.();
      }
      reject(error);
    };
    const onAbort = () => {
      // Removing the frame cancels a navigation that has not started yet.
      settle(true);
      reject(ApiError.aborted(signal?.reason));
    };
    frame.addEventListener('load', onLoad);
    signal?.addEventListener('abort', onAbort);
    const poll = setInterval(() => {
      if (readCookie(cookieName, document.cookie) === undefined) {
        return;
      }
      document.cookie = `${cookieName}=; Max-Age=0; Path=/; SameSite=Strict`;
      // The browser owns the download now; the frame goes later.
      settle(false);
      setTimeout(() => {
        frame.remove();
      }, cleanupMs);
      resolve();
    }, pollMs);
    const timer = setTimeout(() => {
      settle(true);
      reject(
        new ApiError({
          title: 'Download timeout',
          status: 0,
          detail: 'The server did not start the download in time.',
          code: ClientErrorCode.Network,
        }),
      );
    }, options.timeoutMs ?? defaultTimeoutMs);

    const form = document.createElement('form');
    form.method = 'post';
    form.action = url;
    form.enctype = 'application/x-www-form-urlencoded';
    form.target = frame.name;
    form.hidden = true;
    const values: Record<string, string> = { ...fields, [downloadTokenField]: token };
    const csrf = deps.csrfToken();
    if (csrf !== undefined && csrf !== '') {
      values[csrfFormField] = csrf;
    }
    for (const [name, value] of Object.entries(values)) {
      const input = document.createElement('input');
      input.type = 'hidden';
      input.name = name;
      input.value = value;
      form.appendChild(input);
    }
    document.body.appendChild(form);
    try {
      form.submit();
    } catch (cause) {
      settle(true);
      reject(ApiError.network(cause));
    } finally {
      form.remove();
    }
  });
}
