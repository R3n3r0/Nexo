/**
 * A located error of a malformed policy document (schema PolicyError):
 * 1-based line and column in the submitted JSON text, 0 when unknown.
 */
export interface LocatedError {
  /** 1-based statement index, 0 for document-level errors. */
  statement: number;
  sid?: string;
  element?: string;
  line: number;
  column: number;
  message: string;
}

/**
 * An invalid rule of a bucket configuration (lifecycle, event
 * subscriptions, CORS): schema RuleError of the invalid_configuration
 * problems.
 */
export interface RuleProblem {
  /** 1-based index of the rule, 0 for the whole configuration. */
  rule: number;
  id?: string;
  /** S3 error code (InvalidArgument, MalformedXML, NotImplemented...). */
  code: string;
  message: string;
}

/**
 * A located problem of an access rule (schema AccessRuleError of the
 * invalid_access_rule, too_many_access_rules and public_access_blocked
 * problems).
 */
export interface AccessRuleProblem {
  /** 1-based index of the rule, 0 for the whole list. */
  rule: number;
  id?: string;
  /** The element at fault (match.pattern, conditions.sourceIp[1]...). */
  field?: string;
  /** 1-based position of the problem in the pattern. */
  position?: number;
  message: string;
}

/**
 * RFC 9457 problem details document as produced by the Admin API
 * (httpx.Problem in the Go transport).
 */
export interface ProblemDetails {
  type?: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  /** Stable machine-readable code (snake_case), e.g. "bucket_not_found". */
  code: string;
  /** Request id to correlate with the server logs. */
  requestId?: string;
  /** Located errors of a malformed policy (code malformed_policy). */
  errors?: LocatedError[];
  /** Invalid rules of a bucket configuration (code invalid_configuration). */
  ruleErrors?: RuleProblem[];
  /** The invalid access rule (codes invalid_access_rule, too_many_access_rules...). */
  accessRuleErrors?: AccessRuleProblem[];
}

/** Codes used by the client for failures that never reached a problem document. */
export const ClientErrorCode = {
  Network: 'network_error',
  Http: 'http_error',
  InvalidResponse: 'invalid_response',
  /** The request was cancelled by the caller (AbortSignal). */
  Aborted: 'aborted',
} as const;

/**
 * ApiError is the single error type surfaced by every service implementation.
 * HTTP services build it from problem+json responses; mock services build it
 * directly so that components can treat both transports identically.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly title: string;
  readonly detail: string | undefined;
  readonly instance: string | undefined;
  readonly requestId: string | undefined;
  /** Located errors of a malformed policy; empty for every other problem. */
  readonly errors: readonly LocatedError[];
  /** Invalid rules of a bucket configuration; empty for every other problem. */
  readonly ruleErrors: readonly RuleProblem[];
  /** The located problems of invalid access rules; empty for every other problem. */
  readonly accessRuleErrors: readonly AccessRuleProblem[];

  constructor(problem: ProblemDetails, options?: ErrorOptions) {
    super(problem.detail ?? problem.title, options);
    this.name = 'ApiError';
    this.status = problem.status;
    this.code = problem.code;
    this.title = problem.title;
    this.detail = problem.detail;
    this.instance = problem.instance;
    this.requestId = problem.requestId;
    this.errors = problem.errors ?? [];
    this.ruleErrors = problem.ruleErrors ?? [];
    this.accessRuleErrors = problem.accessRuleErrors ?? [];
  }

  /** Builds an error from a parsed problem+json body, defaulting the status. */
  static fromProblem(body: Partial<ProblemDetails>, fallbackStatus: number): ApiError {
    const status = typeof body.status === 'number' ? body.status : fallbackStatus;
    return new ApiError({
      type: body.type,
      title: body.title ?? defaultTitle(status),
      status,
      detail: body.detail,
      instance: body.instance,
      code: body.code ?? ClientErrorCode.Http,
      requestId: body.requestId,
      errors: Array.isArray(body.errors) ? body.errors.filter(isLocatedError) : undefined,
      ruleErrors: Array.isArray(body.ruleErrors)
        ? body.ruleErrors.filter(isRuleProblem)
        : undefined,
      accessRuleErrors: Array.isArray(body.accessRuleErrors)
        ? body.accessRuleErrors.filter(isAccessRuleProblem)
        : undefined,
    });
  }

  /** Builds an error for a non-2xx response without a problem document. */
  static fromStatus(status: number, statusText?: string): ApiError {
    return new ApiError({
      title: statusText && statusText !== '' ? statusText : defaultTitle(status),
      status,
      code: ClientErrorCode.Http,
    });
  }

  /** Builds an error for a request cancelled through its AbortSignal. */
  static aborted(cause?: unknown): ApiError {
    return new ApiError(
      {
        title: 'Aborted',
        status: 0,
        detail: 'The request was cancelled.',
        code: ClientErrorCode.Aborted,
      },
      { cause },
    );
  }

  /** Builds an error for a request that never got a response. */
  static network(cause: unknown): ApiError {
    const detail = cause instanceof Error ? cause.message : String(cause);
    return new ApiError(
      { title: 'Network error', status: 0, detail, code: ClientErrorCode.Network },
      { cause },
    );
  }

  get isUnauthorized(): boolean {
    return this.status === 401;
  }

  get isForbidden(): boolean {
    return this.status === 403;
  }

  get isNotFound(): boolean {
    return this.status === 404;
  }

  get isConflict(): boolean {
    return this.status === 409;
  }

  /**
   * True when a conditional change lost a race: the document changed since
   * it was read (412 precondition_failed on If-Match).
   */
  get isPreconditionFailed(): boolean {
    return this.status === 412;
  }

  /** True when the server throttles the caller (429 too_many_requests). */
  get isTooManyRequests(): boolean {
    return this.status === 429;
  }

  /** True when the caller cancelled the request. */
  get isAborted(): boolean {
    return this.code === ClientErrorCode.Aborted;
  }

  /** True when the request never reached the server or the response was unusable. */
  get isNetwork(): boolean {
    return this.status === 0;
  }
}

function isLocatedError(value: unknown): value is LocatedError {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  const v = value as Record<string, unknown>;
  return (
    typeof v.message === 'string' &&
    typeof v.line === 'number' &&
    typeof v.column === 'number' &&
    typeof v.statement === 'number'
  );
}

function isRuleProblem(value: unknown): value is RuleProblem {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  const v = value as Record<string, unknown>;
  return typeof v.rule === 'number' && typeof v.code === 'string' && typeof v.message === 'string';
}

function isAccessRuleProblem(value: unknown): value is AccessRuleProblem {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  const v = value as Record<string, unknown>;
  return typeof v.rule === 'number' && typeof v.message === 'string';
}

export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError;
}

const titles: Record<number, string> = {
  400: 'Bad Request',
  401: 'Unauthorized',
  403: 'Forbidden',
  404: 'Not Found',
  409: 'Conflict',
  412: 'Precondition Failed',
  500: 'Internal Server Error',
  502: 'Bad Gateway',
  503: 'Service Unavailable',
};

function defaultTitle(status: number): string {
  return titles[status] ?? `HTTP ${status}`;
}
