/**
 * A small policy evaluator for the in-memory mocks: enough of the AWS
 * semantics (explicit deny wins, then allow, otherwise implicit deny;
 * Action/NotAction, Resource/NotResource, the common condition operators)
 * for the simulator and the effective permissions to behave plausibly in
 * the demo mode and in the component tests. The server engine is the
 * reference implementation.
 */
import { wildcardMatch } from '@/lib/policyLint';

import type {
  EvaluatedStatement,
  PolicyDocument,
  PolicySource,
  PolicyStatement,
  Simulation,
} from '../types';

/** A document taking part in an evaluation, with its origin. */
export interface SourcedDocument {
  source: PolicySource;
  document: PolicyDocument;
  policyName?: string;
  group?: string;
}

export interface EvaluationRequest {
  user: string;
  action: string;
  bucket?: string;
  object?: string;
  /** Condition values by key (aws:SourceIp, aws:SecureTransport, ...). */
  context: Record<string, string[]>;
}

const list = (value: string | string[] | undefined): string[] =>
  value === undefined ? [] : Array.isArray(value) ? value : [value];

/** The ARN of the request: bucket, object or none for global actions. */
export function requestArn(bucket?: string, object?: string): string | undefined {
  if (bucket === undefined || bucket === '') {
    return undefined;
  }
  return object !== undefined && object !== ''
    ? `arn:aws:s3:::${bucket}/${object}`
    : `arn:aws:s3:::${bucket}`;
}

function ipv4ToNumber(ip: string): number | undefined {
  const parts = ip.split('.');
  if (parts.length !== 4) return undefined;
  let out = 0;
  for (const part of parts) {
    const n = Number(part);
    if (!/^\d{1,3}$/.test(part) || n > 255) return undefined;
    out = out * 256 + n;
  }
  return out;
}

function ipInCidr(ip: string, cidr: string): boolean {
  const [network, bitsText] = cidr.includes('/') ? cidr.split('/') : [cidr, '32'];
  const ipNum = ipv4ToNumber(ip);
  const netNum = ipv4ToNumber(network ?? '');
  const bits = Number(bitsText);
  if (ipNum === undefined || netNum === undefined || !(bits >= 0 && bits <= 32)) {
    return false;
  }
  const size = 2 ** (32 - bits);
  return Math.floor(ipNum / size) === Math.floor(netNum / size);
}

/** Evaluates one operator on the request values; undefined for unsupported operators. */
function conditionMatches(
  operator: string,
  expected: string[],
  actual: string[] | undefined,
): boolean | undefined {
  let op = operator.replace(/^(ForAllValues|ForAnyValue):/, '');
  const ifExists = op.endsWith('IfExists');
  if (ifExists) op = op.slice(0, -'IfExists'.length);
  if (op === 'Null') {
    const absent = actual === undefined || actual.length === 0;
    return expected.some((e) => (e === 'true') === absent);
  }
  if (actual === undefined || actual.length === 0) {
    if (ifExists) return true;
    return /Not/.test(op);
  }
  const any = (pred: (a: string, e: string) => boolean) =>
    actual.some((a) => expected.some((e) => pred(a, e)));
  const time = (v: string) => Date.parse(v);
  switch (op) {
    case 'StringEquals':
      return any((a, e) => a === e);
    case 'StringNotEquals':
      return !any((a, e) => a === e);
    case 'StringEqualsIgnoreCase':
      return any((a, e) => a.toLowerCase() === e.toLowerCase());
    case 'StringNotEqualsIgnoreCase':
      return !any((a, e) => a.toLowerCase() === e.toLowerCase());
    case 'StringLike':
      return any((a, e) => wildcardMatch(e, a));
    case 'StringNotLike':
      return !any((a, e) => wildcardMatch(e, a));
    case 'Bool':
      return any((a, e) => a.toLowerCase() === e.toLowerCase());
    case 'IpAddress':
      return any((a, e) => ipInCidr(a, e));
    case 'NotIpAddress':
      return !any((a, e) => ipInCidr(a, e));
    case 'NumericEquals':
      return any((a, e) => Number(a) === Number(e));
    case 'NumericLessThan':
      return any((a, e) => Number(a) < Number(e));
    case 'NumericGreaterThan':
      return any((a, e) => Number(a) > Number(e));
    case 'DateLessThan':
      return any((a, e) => time(a) < time(e));
    case 'DateGreaterThan':
      return any((a, e) => time(a) > time(e));
    default:
      return undefined;
  }
}

type Mismatch = EvaluatedStatement['mismatch'];

function statementMismatch(statement: PolicyStatement, req: EvaluationRequest): Mismatch {
  const actions = list(statement.Action);
  const notActions = list(statement.NotAction);
  const actionHit =
    actions.length > 0
      ? actions.some((a) => wildcardMatch(a, req.action, true))
      : !notActions.some((a) => wildcardMatch(a, req.action, true));
  if (!actionHit) return 'action';

  const arn = requestArn(req.bucket, req.object);
  if (arn !== undefined) {
    const resources = list(statement.Resource).map((r) =>
      r.replace(/\$\{aws:username\}/g, req.user),
    );
    const notResources = list(statement.NotResource);
    const resourceHit =
      resources.length > 0
        ? resources.some((r) => wildcardMatch(r, arn))
        : notResources.length > 0
          ? !notResources.some((r) => wildcardMatch(r, arn))
          : true;
    if (!resourceHit) return 'resource';
  }

  for (const [operator, keys] of Object.entries(statement.Condition ?? {})) {
    for (const [key, values] of Object.entries(keys)) {
      const result = conditionMatches(operator, list(values), req.context[key]);
      if (result !== true) return 'condition';
    }
  }
  return undefined;
}

/**
 * Evaluates the documents for a request. With `inline`, the result is
 * intersected with the inline policy of a service account.
 */
export function evaluate(
  documents: SourcedDocument[],
  req: EvaluationRequest,
  options: { root?: boolean } = {},
): Simulation {
  const base = {
    user: req.user,
    action: req.action,
    ...(req.bucket !== undefined ? { bucket: req.bucket } : {}),
    ...(req.object !== undefined ? { object: req.object } : {}),
  };
  if (options.root === true) {
    return {
      ...base,
      allowed: true,
      decision: 'allow',
      reason: 'the root user is allowed every action',
      source: 'root',
      evaluated: [],
    };
  }

  const evaluated: EvaluatedStatement[] = [];
  for (const doc of documents) {
    doc.document.Statement.forEach((statement, index) => {
      const mismatch = statementMismatch(statement, req);
      evaluated.push({
        source: doc.source,
        ...(doc.policyName !== undefined ? { policyName: doc.policyName } : {}),
        ...(doc.group !== undefined ? { group: doc.group } : {}),
        index,
        ...(statement.Sid !== undefined ? { sid: statement.Sid } : {}),
        effect: statement.Effect,
        matched: mismatch === undefined,
        ...(mismatch !== undefined ? { mismatch } : {}),
        statement,
      });
    });
  }

  const deciding =
    evaluated.find((e) => e.matched && e.effect === 'Deny') ??
    evaluated.find((e) => e.matched && e.effect === 'Allow' && e.source !== 'inline');
  const inlineDocs = documents.filter((d) => d.source === 'inline');
  const inlineAllows =
    inlineDocs.length === 0 ||
    evaluated.some((e) => e.source === 'inline' && e.matched && e.effect === 'Allow');

  if (deciding === undefined || deciding.effect === 'Deny' || !inlineAllows) {
    const explicit = deciding?.effect === 'Deny';
    return {
      ...base,
      allowed: false,
      decision: 'deny',
      reason: explicit
        ? 'explicitly denied by a statement'
        : deciding !== undefined
          ? 'not allowed by the inline policy of the access key'
          : 'no statement allows the request (implicit deny)',
      ...(explicit ? sourceOf(deciding) : {}),
      evaluated,
    };
  }
  return {
    ...base,
    allowed: true,
    decision: 'allow',
    reason: 'allowed by a statement',
    ...sourceOf(deciding),
    evaluated,
  };
}

function sourceOf(statement: EvaluatedStatement): Partial<Simulation> {
  return {
    source: statement.source,
    ...(statement.policyName !== undefined ? { policyName: statement.policyName } : {}),
    ...(statement.group !== undefined ? { group: statement.group } : {}),
    statement,
  };
}
