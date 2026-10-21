/**
 * Client-side checks of a policy document, mirroring the rules of the Go
 * validator (internal/service/policy/validate.go). The server stays the
 * authority (POST /policies/validate); the lint gives immediate feedback
 * while typing and backs the validation of the in-memory mock services.
 */
import type { LocatedError } from '@/services';

import { parseJson, pointerSegment, type TextPosition } from './json';
import {
  actionServices,
  isConditionOperator,
  nexoActionNames,
  s3ActionNames,
  s3ArnPrefix,
} from './policyCatalog';

/** The only supported policy version. */
export const policyVersion = '2012-10-17';

export type PolicyKind = 'identity' | 'bucket';

export interface LintOptions {
  /** identity (IAM/inline, no Principal) or bucket (Principal required). */
  kind?: PolicyKind;
  /** Bucket of a bucket policy: resources must stay inside it. */
  bucket?: string;
}

type Json = Record<string, unknown>;

const isObject = (value: unknown): value is Json =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

/** Normalises a string-or-strings element; undefined for any other shape. */
function strings(value: unknown): string[] | undefined {
  if (typeof value === 'string') {
    return [value];
  }
  if (Array.isArray(value) && value.every((v): v is string => typeof v === 'string')) {
    return value;
  }
  return undefined;
}

/** Shell-style wildcard match (* and ?), as policy.matchWildcard. */
export function wildcardMatch(pattern: string, value: string, ignoreCase = false): boolean {
  const p = ignoreCase ? pattern.toLowerCase() : pattern;
  const v = ignoreCase ? value.toLowerCase() : value;
  const escaped = p
    .replace(/[.+^${}()|[\]\\]/g, '\\$&')
    .replace(/\*/g, '.*')
    .replace(/\?/g, '.');
  return new RegExp(`^${escaped}$`, 's').test(v);
}

/** Checks one Action/NotAction entry; returns the problem or undefined. */
export function actionProblem(action: string): string | undefined {
  if (action === '*') {
    return undefined;
  }
  const colon = action.indexOf(':');
  const service = colon > 0 ? action.slice(0, colon) : '';
  const name = colon > 0 ? action.slice(colon + 1) : '';
  if (service === '' || name === '' || !/^[A-Za-z0-9*?_-]+$/.test(name)) {
    return `invalid action "${action}" (expected <service>:<action>)`;
  }
  const lower = service.toLowerCase();
  if (!(actionServices as readonly string[]).includes(lower)) {
    return `invalid action "${action}": unknown service "${service}" (s3, nexo, admin, sts, kms)`;
  }
  if (lower === 's3' && !s3ActionNames.some((known) => wildcardMatch(name, known, true))) {
    return `unknown S3 action "${action}"`;
  }
  if (lower === 'nexo' && !nexoActionNames.some((known) => wildcardMatch(name, known, true))) {
    return `unknown Nexo action "${action}"`;
  }
  return undefined;
}

/** Checks one Resource/NotResource entry. */
export function resourceProblem(resource: string): string | undefined {
  if (resource === '*') {
    return undefined;
  }
  if (!resource.startsWith(s3ArnPrefix) || resource.length === s3ArnPrefix.length) {
    return `invalid resource "${resource}" (expected arn:aws:s3:::bucket[/key])`;
  }
  return undefined;
}

/** Actions on the buckets and objects: S3 and the object actions of Nexo. */
const isS3Action = (action: string) => action === '*' || /^(s3|nexo):/i.test(action);

/**
 * Lints a policy JSON text. Syntax errors stop the analysis; every other
 * problem is reported with the position of the element at fault.
 */
export function lintPolicy(text: string, options: LintOptions = {}): LocatedError[] {
  const parsed = parseJson(text);
  if (!parsed.ok) {
    return [
      {
        statement: 0,
        element: 'JSON',
        line: parsed.error.line,
        column: parsed.error.column,
        message: parsed.error.message,
      },
    ];
  }
  const { value, positions } = parsed;
  const errors: LocatedError[] = [];
  const at = (pointer: string): TextPosition =>
    positions.get(pointer) ?? positions.get('') ?? { offset: 0, line: 1, column: 1 };
  const report = (
    pointer: string,
    statement: number,
    element: string,
    message: string,
    sid?: string,
  ) => {
    const pos = at(pointer);
    errors.push({
      statement,
      element,
      line: pos.line,
      column: pos.column,
      message,
      ...(sid !== undefined && sid !== '' ? { sid } : {}),
    });
  };

  if (!isObject(value)) {
    report('', 0, '', 'the policy must be a JSON object');
    return errors;
  }
  if (value.Version !== policyVersion) {
    report(
      value.Version === undefined ? '' : '/Version',
      0,
      'Version',
      `must be "${policyVersion}"`,
    );
  }
  const rawStatements = value.Statement;
  const statements = Array.isArray(rawStatements)
    ? rawStatements
    : isObject(rawStatements)
      ? [rawStatements]
      : undefined;
  const single = !Array.isArray(rawStatements);
  if (statements === undefined || statements.length === 0) {
    report(
      rawStatements === undefined ? '' : '/Statement',
      0,
      'Statement',
      'the policy must contain at least one statement',
    );
    return errors;
  }

  const sids = new Set<string>();
  statements.forEach((st, index) => {
    const base = single ? '/Statement' : `/Statement/${index}`;
    const n = index + 1;
    const ptr = (key: string) => `${base}/${pointerSegment(key)}`;
    if (!isObject(st)) {
      report(base, n, '', 'a statement must be a JSON object');
      return;
    }
    const sid = typeof st.Sid === 'string' ? st.Sid : undefined;
    if (sid !== undefined && sid !== '') {
      if (sids.has(sid)) {
        report(ptr('Sid'), n, 'Sid', `duplicate Sid "${sid}"`, sid);
      }
      sids.add(sid);
    }
    if (st.Effect !== 'Allow' && st.Effect !== 'Deny') {
      report(
        st.Effect === undefined ? base : ptr('Effect'),
        n,
        'Effect',
        'must be "Allow" or "Deny"',
        sid,
      );
    }

    const hasAction = st.Action !== undefined;
    const hasNotAction = st.NotAction !== undefined;
    if (hasAction === hasNotAction) {
      report(base, n, 'Action', 'exactly one of Action and NotAction is required', sid);
    }
    let needsResource = hasNotAction;
    for (const element of ['Action', 'NotAction'] as const) {
      if (st[element] === undefined) continue;
      const list = strings(st[element]);
      if (list === undefined || list.length === 0) {
        report(ptr(element), n, element, 'must be a string or a non-empty array of strings', sid);
        continue;
      }
      list.forEach((action, i) => {
        const problem = actionProblem(action);
        if (problem !== undefined) {
          const p = Array.isArray(st[element]) ? `${ptr(element)}/${i}` : ptr(element);
          report(p, n, element, problem, sid);
        }
        if (isS3Action(action)) needsResource = true;
        if (options.kind === 'bucket' && !isS3Action(action)) {
          report(ptr(element), n, element, `action "${action}" is not an S3 action`, sid);
        }
      });
    }

    const hasResource = st.Resource !== undefined;
    const hasNotResource = st.NotResource !== undefined;
    if (hasResource && hasNotResource) {
      report(
        ptr('NotResource'),
        n,
        'Resource',
        'Resource and NotResource are mutually exclusive',
        sid,
      );
    }
    if (needsResource && !hasResource && !hasNotResource) {
      report(base, n, 'Resource', 'Resource or NotResource is required for S3 actions', sid);
    }
    for (const element of ['Resource', 'NotResource'] as const) {
      if (st[element] === undefined) continue;
      const list = strings(st[element]);
      if (list === undefined || list.length === 0) {
        report(ptr(element), n, element, 'must be a string or a non-empty array of strings', sid);
        continue;
      }
      list.forEach((resource, i) => {
        const p = Array.isArray(st[element]) ? `${ptr(element)}/${i}` : ptr(element);
        const problem = resourceProblem(resource);
        if (problem !== undefined) {
          report(p, n, element, problem, sid);
        } else if (options.kind === 'bucket' && options.bucket !== undefined) {
          const arn = `${s3ArnPrefix}${options.bucket}`;
          if (resource !== arn && !resource.startsWith(`${arn}/`)) {
            report(
              p,
              n,
              element,
              `resource "${resource}" is outside bucket ${options.bucket}`,
              sid,
            );
          }
        }
      });
    }

    const hasPrincipal = st.Principal !== undefined || st.NotPrincipal !== undefined;
    if (options.kind === 'bucket' && !hasPrincipal) {
      report(
        base,
        n,
        'Principal',
        'bucket policy statements must name a Principal or NotPrincipal',
        sid,
      );
    }
    if (options.kind !== 'bucket' && hasPrincipal) {
      report(
        ptr(st.Principal !== undefined ? 'Principal' : 'NotPrincipal'),
        n,
        'Principal',
        'identity policies must not name a Principal',
        sid,
      );
    }

    if (st.Condition !== undefined) {
      if (!isObject(st.Condition)) {
        report(ptr('Condition'), n, 'Condition', 'must be an object of operators', sid);
      } else {
        for (const [op, keys] of Object.entries(st.Condition)) {
          const opPtr = `${ptr('Condition')}/${pointerSegment(op)}`;
          if (!isConditionOperator(op)) {
            report(opPtr, n, `Condition.${op}`, `unknown condition operator "${op}"`, sid);
            continue;
          }
          if (!isObject(keys) || Object.keys(keys).length === 0) {
            report(opPtr, n, `Condition.${op}`, `operator ${op} has no condition keys`, sid);
            continue;
          }
          for (const [key, values] of Object.entries(keys)) {
            const keyPtr = `${opPtr}/${pointerSegment(key)}`;
            if (key.trim() === '' || /[\s${}]/.test(key)) {
              report(keyPtr, n, `Condition.${op}`, `invalid condition key "${key}"`, sid);
              continue;
            }
            const list =
              strings(values) ??
              (typeof values === 'boolean' || typeof values === 'number'
                ? [String(values)]
                : undefined);
            if (list === undefined || list.length === 0) {
              report(keyPtr, n, `Condition.${op}.${key}`, 'must have at least one value', sid);
            }
          }
        }
      }
    }
  });
  return errors;
}
