/**
 * CORS rules of a bucket in the console: the form of the rule editor and
 * the validations of the server (domain.CORSConfiguration.Validate).
 */
import type { CorsRule } from '@/services';

/** The methods a CORS rule may allow. */
export const corsMethods = ['GET', 'PUT', 'POST', 'DELETE', 'HEAD'] as const;
export type CorsMethod = (typeof corsMethods)[number];

/** Limits of the server (domain.MaxCORSRules, domain.MaxCORSRuleIDLength). */
export const corsLimits = { rules: 100, idLength: 255 } as const;

/** The editor of one rule: the lists as one entry per line. */
export interface CorsRuleForm {
  id: string;
  origins: string;
  methods: CorsMethod[];
  allowedHeaders: string;
  exposeHeaders: string;
  maxAgeSeconds: number | '';
}

/** Problems of a rule (translated under cors.issues.*). */
export type CorsIssue = 'idTooLong' | 'origins' | 'wildcards' | 'methods' | 'headers' | 'maxAge';

export interface CorsFormIssues {
  id?: CorsIssue;
  origins?: CorsIssue;
  methods?: CorsIssue;
  allowedHeaders?: CorsIssue;
  maxAgeSeconds?: CorsIssue;
}

/** A new rule: GET and HEAD from any origin. */
export function emptyCorsForm(): CorsRuleForm {
  return {
    id: '',
    origins: '*',
    methods: ['GET', 'HEAD'],
    allowedHeaders: '',
    exposeHeaders: '',
    maxAgeSeconds: '',
  };
}

/** The entries of a list field: one per line or comma separated, trimmed. */
export function listOf(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((v) => v.trim())
    .filter((v) => v !== '');
}

export function corsRuleToForm(rule: CorsRule): CorsRuleForm {
  return {
    id: rule.id ?? '',
    origins: rule.allowedOrigins.join('\n'),
    methods: rule.allowedMethods.filter((m): m is CorsMethod =>
      (corsMethods as readonly string[]).includes(m),
    ),
    allowedHeaders: (rule.allowedHeaders ?? []).join('\n'),
    exposeHeaders: (rule.exposeHeaders ?? []).join('\n'),
    maxAgeSeconds: rule.maxAgeSeconds ?? '',
  };
}

export function corsFormToRule(form: CorsRuleForm): CorsRule {
  const allowedHeaders = listOf(form.allowedHeaders);
  const exposeHeaders = listOf(form.exposeHeaders);
  return {
    ...(form.id.trim() !== '' ? { id: form.id.trim() } : {}),
    allowedOrigins: listOf(form.origins),
    allowedMethods: corsMethods.filter((m) => form.methods.includes(m)),
    ...(allowedHeaders.length > 0 ? { allowedHeaders } : {}),
    ...(exposeHeaders.length > 0 ? { exposeHeaders } : {}),
    ...(form.maxAgeSeconds !== '' ? { maxAgeSeconds: form.maxAgeSeconds } : {}),
  };
}

const wildcards = (v: string) => v.split('*').length - 1;

/** Validates a rule as the server does. */
export function validateCorsRule(rule: CorsRule): CorsFormIssues {
  const issues: CorsFormIssues = {};
  if ([...(rule.id ?? '')].length > corsLimits.idLength) {
    issues.id = 'idTooLong';
  }
  if (rule.allowedOrigins.length === 0) {
    issues.origins = 'origins';
  } else if (rule.allowedOrigins.some((o) => wildcards(o) > 1)) {
    issues.origins = 'wildcards';
  }
  if (
    rule.allowedMethods.length === 0 ||
    rule.allowedMethods.some((m) => !(corsMethods as readonly string[]).includes(m))
  ) {
    issues.methods = 'methods';
  }
  if ((rule.allowedHeaders ?? []).some((h) => wildcards(h) > 1)) {
    issues.allowedHeaders = 'headers';
  }
  if (
    rule.maxAgeSeconds !== undefined &&
    (!Number.isInteger(rule.maxAgeSeconds) || rule.maxAgeSeconds < 0)
  ) {
    issues.maxAgeSeconds = 'maxAge';
  }
  return issues;
}

/** True when a rule has no problem. */
export function isValidCorsRule(rule: CorsRule): boolean {
  const issues = validateCorsRule(rule);
  return (
    issues.id === undefined &&
    issues.origins === undefined &&
    issues.methods === undefined &&
    issues.allowedHeaders === undefined &&
    issues.maxAgeSeconds === undefined
  );
}
