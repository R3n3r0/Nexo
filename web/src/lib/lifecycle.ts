/**
 * Lifecycle rules in the console (D-BKT-03, TD-U-08): the form model of
 * the guided editor, its conversion to and from the rules of the Admin API
 * (LifecycleRule), the S3 XML document (LifecycleConfiguration) and the
 * JSON view, and the validations of the server (internal/service/ilm)
 * applied before sending.
 */
import type {
  LifecycleConfiguration,
  LifecycleFilter,
  LifecycleNoncurrentTransition,
  LifecycleRule,
  LifecycleTransition,
} from '@/services';

import { parseJson } from './json';

/** XML namespace of the S3 documents. */
export const s3Namespace = 'http://s3.amazonaws.com/doc/2006-03-01/';

/** Limits of the server (internal/service/ilm). */
export const lifecycleLimits = {
  rules: 1000,
  idLength: 255,
  prefixLength: 1024,
  tagKeyLength: 128,
  tagValueLength: 256,
  newerNoncurrentVersions: 100,
} as const;

/** The expiration of the current versions chosen in the form. */
export type ExpirationKind = 'none' | 'days' | 'date' | 'deleteMarker';

/** A number field of the form: '' when empty. */
export type NumberField = number | '';

/** One tag of the filter of a rule. */
export interface TagField {
  key: string;
  value: string;
}

/** The guided editor of one rule. */
export interface LifecycleRuleForm {
  id: string;
  enabled: boolean;
  prefix: string;
  tags: TagField[];
  /** Exclusive size bounds in bytes. */
  sizeGreaterThan: NumberField;
  sizeLessThan: NumberField;
  expiration: ExpirationKind;
  expirationDays: NumberField;
  /** ISO date (YYYY-MM-DD), midnight UTC. */
  expirationDate: string;
  noncurrent: boolean;
  noncurrentDays: NumberField;
  newerNoncurrentVersions: NumberField;
  abortUploads: boolean;
  abortDays: NumberField;
  /** Actions the form does not edit (remote tiers, P2), kept as they are. */
  transitions?: LifecycleTransition[];
  noncurrentTransitions?: LifecycleNoncurrentTransition[];
}

/** A new rule: enabled, the whole bucket, current versions expire after 30 days. */
export function emptyRuleForm(): LifecycleRuleForm {
  return {
    id: '',
    enabled: true,
    prefix: '',
    tags: [],
    sizeGreaterThan: '',
    sizeLessThan: '',
    expiration: 'days',
    expirationDays: 30,
    expirationDate: '',
    noncurrent: false,
    noncurrentDays: 30,
    newerNoncurrentVersions: '',
    abortUploads: false,
    abortDays: 7,
  };
}

const num = (value: number | undefined): NumberField => (value === undefined ? '' : value);

/** The date part of an ISO 8601 date or timestamp ("2030-01-01T00:00:00Z" → "2030-01-01"). */
function datePart(value: string): string {
  return value.trim().slice(0, 10);
}

/** The form of a rule of the Admin API. */
export function ruleToForm(rule: LifecycleRule): LifecycleRuleForm {
  const form = emptyRuleForm();
  form.id = rule.id ?? '';
  form.enabled = rule.enabled !== false;
  form.prefix = rule.filter?.prefix ?? '';
  form.tags = Object.entries(rule.filter?.tags ?? {}).map(([key, value]) => ({ key, value }));
  form.sizeGreaterThan = num(rule.filter?.sizeGreaterThan);
  form.sizeLessThan = num(rule.filter?.sizeLessThan);
  const exp = rule.expiration;
  form.expiration = 'none';
  form.expirationDays = '';
  if (exp?.expiredObjectDeleteMarker === true) {
    form.expiration = 'deleteMarker';
  } else if (exp?.date !== undefined && exp.date !== '') {
    form.expiration = 'date';
    form.expirationDate = datePart(exp.date);
  } else if (exp?.days !== undefined) {
    form.expiration = 'days';
    form.expirationDays = exp.days;
  }
  form.noncurrent = rule.noncurrentExpiration !== undefined;
  form.noncurrentDays = num(rule.noncurrentExpiration?.noncurrentDays);
  form.newerNoncurrentVersions = num(rule.noncurrentExpiration?.newerNoncurrentVersions);
  form.abortUploads = rule.abortIncompleteUpload !== undefined;
  form.abortDays = num(rule.abortIncompleteUpload?.daysAfterInitiation);
  if (rule.transitions !== undefined && rule.transitions.length > 0) {
    form.transitions = rule.transitions;
  }
  if (rule.noncurrentTransitions !== undefined && rule.noncurrentTransitions.length > 0) {
    form.noncurrentTransitions = rule.noncurrentTransitions;
  }
  return form;
}

/** The rule of the Admin API of a form; empty fields are left out. */
export function formToRule(form: LifecycleRuleForm): LifecycleRule {
  const rule: LifecycleRule = {};
  const id = form.id.trim();
  if (id !== '') {
    rule.id = id;
  }
  rule.enabled = form.enabled;
  const filter: LifecycleFilter = {};
  if (form.prefix !== '') {
    filter.prefix = form.prefix;
  }
  const tags = form.tags.filter((t) => t.key.trim() !== '');
  if (tags.length > 0) {
    filter.tags = Object.fromEntries(tags.map((t) => [t.key.trim(), t.value]));
  }
  if (form.sizeGreaterThan !== '') {
    filter.sizeGreaterThan = form.sizeGreaterThan;
  }
  if (form.sizeLessThan !== '') {
    filter.sizeLessThan = form.sizeLessThan;
  }
  if (Object.keys(filter).length > 0) {
    rule.filter = filter;
  }
  switch (form.expiration) {
    case 'days':
      rule.expiration = form.expirationDays === '' ? {} : { days: form.expirationDays };
      break;
    case 'date':
      rule.expiration = { date: form.expirationDate };
      break;
    case 'deleteMarker':
      rule.expiration = { expiredObjectDeleteMarker: true };
      break;
    case 'none':
      break;
  }
  if (form.noncurrent) {
    rule.noncurrentExpiration = {
      ...(form.noncurrentDays !== '' ? { noncurrentDays: form.noncurrentDays } : {}),
      ...(form.newerNoncurrentVersions !== ''
        ? { newerNoncurrentVersions: form.newerNoncurrentVersions }
        : {}),
    };
  }
  if (form.abortUploads) {
    rule.abortIncompleteUpload =
      form.abortDays === '' ? {} : { daysAfterInitiation: form.abortDays };
  }
  if (form.transitions !== undefined) {
    rule.transitions = form.transitions;
  }
  if (form.noncurrentTransitions !== undefined) {
    rule.noncurrentTransitions = form.noncurrentTransitions;
  }
  return rule;
}

/* ------------------------------------------------------------------------ */
/* Validation (the rules of internal/service/ilm, before sending).          */
/* ------------------------------------------------------------------------ */

/** Problem codes of a rule (translated under lifecycle.issues.*). */
export type RuleIssue =
  | 'idTooLong'
  | 'idDuplicate'
  | 'prefixTooLong'
  | 'tagKey'
  | 'tagDuplicate'
  | 'tagTooLong'
  | 'sizeNegative'
  | 'sizeRange'
  | 'positive'
  | 'date'
  | 'expirationOne'
  | 'newerRange'
  | 'noAction'
  | 'abortWithFilter'
  | 'deleteMarkerWithTags';

/** The problems of a form, by field. */
export interface RuleFormIssues {
  id?: RuleIssue;
  prefix?: RuleIssue;
  tags?: RuleIssue;
  sizeGreaterThan?: RuleIssue;
  sizeLessThan?: RuleIssue;
  expirationDays?: RuleIssue;
  expirationDate?: RuleIssue;
  noncurrentDays?: RuleIssue;
  newerNoncurrentVersions?: RuleIssue;
  abortDays?: RuleIssue;
  /** Problems of the rule as a whole (no action, incompatible filter). */
  rule?: RuleIssue;
}

const isPositiveInteger = (v: NumberField) => v !== '' && Number.isInteger(v) && v > 0;

/** True for an ISO date at midnight UTC ("2030-01-01" or "2030-01-01T00:00:00Z"). */
export function isMidnightDate(value: string): boolean {
  const v = value.trim();
  if (!/^\d{4}-\d{2}-\d{2}(T00:00:00(\.0+)?Z)?$/.test(v)) {
    return false;
  }
  const t = Date.parse(`${v.slice(0, 10)}T00:00:00Z`);
  return !Number.isNaN(t) && new Date(t).toISOString().slice(0, 10) === v.slice(0, 10);
}

/**
 * Validates a form like the server validates the rule (days > 0, one
 * expiration, filters, at least one action). otherIds are the ids of the
 * other rules of the configuration.
 */
export function validateRuleForm(
  form: LifecycleRuleForm,
  otherIds: readonly string[] = [],
): RuleFormIssues {
  const issues: RuleFormIssues = {};
  const id = form.id.trim();
  if (id.length > lifecycleLimits.idLength) {
    issues.id = 'idTooLong';
  } else if (id !== '' && otherIds.includes(id)) {
    issues.id = 'idDuplicate';
  }
  if (form.prefix.length > lifecycleLimits.prefixLength) {
    issues.prefix = 'prefixTooLong';
  }
  const keys = form.tags.map((t) => t.key.trim());
  if (keys.some((k) => k === '')) {
    issues.tags = 'tagKey';
  } else if (new Set(keys).size !== keys.length) {
    issues.tags = 'tagDuplicate';
  } else if (
    form.tags.some(
      (t) =>
        t.key.trim().length > lifecycleLimits.tagKeyLength ||
        t.value.length > lifecycleLimits.tagValueLength,
    )
  ) {
    issues.tags = 'tagTooLong';
  }
  for (const field of ['sizeGreaterThan', 'sizeLessThan'] as const) {
    const v = form[field];
    if (v !== '' && (!Number.isInteger(v) || v < 0)) {
      issues[field] = 'sizeNegative';
    }
  }
  if (
    issues.sizeLessThan === undefined &&
    form.sizeGreaterThan !== '' &&
    form.sizeLessThan !== '' &&
    form.sizeGreaterThan >= form.sizeLessThan
  ) {
    issues.sizeLessThan = 'sizeRange';
  }
  if (form.expiration === 'days' && !isPositiveInteger(form.expirationDays)) {
    issues.expirationDays = 'positive';
  }
  if (form.expiration === 'date' && !isMidnightDate(form.expirationDate)) {
    issues.expirationDate = 'date';
  }
  if (form.noncurrent) {
    if (!isPositiveInteger(form.noncurrentDays)) {
      issues.noncurrentDays = 'positive';
    }
    const newer = form.newerNoncurrentVersions;
    if (
      newer !== '' &&
      (!Number.isInteger(newer) || newer < 1 || newer > lifecycleLimits.newerNoncurrentVersions)
    ) {
      issues.newerNoncurrentVersions = 'newerRange';
    }
  }
  if (form.abortUploads && !isPositiveInteger(form.abortDays)) {
    issues.abortDays = 'positive';
  }
  const hasTags = form.tags.length > 0;
  const hasSize = form.sizeGreaterThan !== '' || form.sizeLessThan !== '';
  if (
    form.expiration === 'none' &&
    !form.noncurrent &&
    !form.abortUploads &&
    (form.transitions?.length ?? 0) === 0 &&
    (form.noncurrentTransitions?.length ?? 0) === 0
  ) {
    issues.rule = 'noAction';
  } else if (form.abortUploads && (hasTags || hasSize)) {
    issues.rule = 'abortWithFilter';
  } else if (form.expiration === 'deleteMarker' && hasTags) {
    issues.rule = 'deleteMarkerWithTags';
  }
  return issues;
}

/** True when the form has no problem. */
export function isValidRuleForm(issues: RuleFormIssues): boolean {
  return Object.values(issues).every((v) => v === undefined);
}

/**
 * The problems of a rule of the Admin API (the mock services validate
 * with it, as the server does).
 */
export function validateRule(rule: LifecycleRule, otherIds: readonly string[] = []): RuleIssue[] {
  const exp = rule.expiration;
  const set = [
    exp?.days !== undefined,
    exp?.date !== undefined && exp.date !== '',
    exp?.expiredObjectDeleteMarker === true,
  ].filter(Boolean).length;
  const issues = Object.values(validateRuleForm(ruleToForm(rule), otherIds)).filter(
    (v): v is RuleIssue => v !== undefined,
  );
  if (set > 1) {
    issues.push('expirationOne');
  }
  if (
    rule.noncurrentExpiration !== undefined &&
    rule.noncurrentExpiration.noncurrentDays === undefined
  ) {
    issues.push('positive');
  }
  return [...new Set(issues)];
}

/* ------------------------------------------------------------------------ */
/* S3 XML (LifecycleConfiguration).                                         */
/* ------------------------------------------------------------------------ */

function escapeXml(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&apos;');
}

function element(name: string, value: string | number | boolean, indent: string): string {
  return `${indent}<${name}>${escapeXml(String(value))}</${name}>`;
}

/** The S3 date of a rule: midnight UTC as a timestamp. */
function xmlDate(value: string): string {
  const v = value.trim();
  return /^\d{4}-\d{2}-\d{2}$/.test(v) ? `${v}T00:00:00Z` : v;
}

function filterXml(filter: LifecycleFilter | undefined, indent: string): string[] {
  const inner = `${indent}  `;
  const parts: string[] = [];
  const prefix = filter?.prefix ?? '';
  const tags = Object.entries(filter?.tags ?? {});
  const criteria =
    (prefix !== '' ? 1 : 0) +
    tags.length +
    (filter?.sizeGreaterThan !== undefined ? 1 : 0) +
    (filter?.sizeLessThan !== undefined ? 1 : 0);
  if (criteria === 0) {
    return [`${indent}<Filter></Filter>`];
  }
  const body = (pad: string) => {
    const out: string[] = [];
    if (prefix !== '') out.push(element('Prefix', prefix, pad));
    for (const [key, value] of tags) {
      out.push(
        `${pad}<Tag>`,
        element('Key', key, `${pad}  `),
        element('Value', value, `${pad}  `),
        `${pad}</Tag>`,
      );
    }
    if (filter?.sizeGreaterThan !== undefined) {
      out.push(element('ObjectSizeGreaterThan', filter.sizeGreaterThan, pad));
    }
    if (filter?.sizeLessThan !== undefined) {
      out.push(element('ObjectSizeLessThan', filter.sizeLessThan, pad));
    }
    return out;
  };
  parts.push(`${indent}<Filter>`);
  if (criteria > 1) {
    parts.push(`${inner}<And>`, ...body(`${inner}  `), `${inner}</And>`);
  } else {
    parts.push(...body(inner));
  }
  parts.push(`${indent}</Filter>`);
  return parts;
}

function ruleXml(rule: LifecycleRule, indent: string): string[] {
  const inner = `${indent}  `;
  const deep = `${inner}  `;
  const out = [`${indent}<Rule>`];
  if (rule.id !== undefined && rule.id !== '') {
    out.push(element('ID', rule.id, inner));
  }
  out.push(...filterXml(rule.filter, inner));
  out.push(element('Status', rule.enabled === false ? 'Disabled' : 'Enabled', inner));
  const exp = rule.expiration;
  if (exp !== undefined) {
    out.push(`${inner}<Expiration>`);
    if (exp.days !== undefined) out.push(element('Days', exp.days, deep));
    if (exp.date !== undefined && exp.date !== '')
      out.push(element('Date', xmlDate(exp.date), deep));
    if (exp.expiredObjectDeleteMarker !== undefined) {
      out.push(element('ExpiredObjectDeleteMarker', exp.expiredObjectDeleteMarker, deep));
    }
    out.push(`${inner}</Expiration>`);
  }
  for (const t of rule.transitions ?? []) {
    out.push(`${inner}<Transition>`);
    if (t.days !== undefined) out.push(element('Days', t.days, deep));
    if (t.date !== undefined && t.date !== '') out.push(element('Date', xmlDate(t.date), deep));
    out.push(element('StorageClass', t.storageClass, deep), `${inner}</Transition>`);
  }
  const nc = rule.noncurrentExpiration;
  if (nc !== undefined) {
    out.push(`${inner}<NoncurrentVersionExpiration>`);
    if (nc.noncurrentDays !== undefined)
      out.push(element('NoncurrentDays', nc.noncurrentDays, deep));
    if (nc.newerNoncurrentVersions !== undefined) {
      out.push(element('NewerNoncurrentVersions', nc.newerNoncurrentVersions, deep));
    }
    out.push(`${inner}</NoncurrentVersionExpiration>`);
  }
  for (const t of rule.noncurrentTransitions ?? []) {
    out.push(`${inner}<NoncurrentVersionTransition>`);
    if (t.noncurrentDays !== undefined) out.push(element('NoncurrentDays', t.noncurrentDays, deep));
    if (t.newerNoncurrentVersions !== undefined) {
      out.push(element('NewerNoncurrentVersions', t.newerNoncurrentVersions, deep));
    }
    out.push(
      element('StorageClass', t.storageClass, deep),
      `${inner}</NoncurrentVersionTransition>`,
    );
  }
  const abort = rule.abortIncompleteUpload;
  if (abort !== undefined) {
    out.push(`${inner}<AbortIncompleteMultipartUpload>`);
    if (abort.daysAfterInitiation !== undefined) {
      out.push(element('DaysAfterInitiation', abort.daysAfterInitiation, deep));
    }
    out.push(`${inner}</AbortIncompleteMultipartUpload>`);
  }
  out.push(`${indent}</Rule>`);
  return out;
}

/** The S3 LifecycleConfiguration document of the rules, indented. */
export function lifecycleToXml(config: LifecycleConfiguration): string {
  return [
    `<LifecycleConfiguration xmlns="${s3Namespace}">`,
    ...config.rules.flatMap((rule) => ruleXml(rule, '  ')),
    '</LifecycleConfiguration>',
  ].join('\n');
}

/** The result of the parsing of a text view. */
export type ParseResult =
  { ok: true; config: LifecycleConfiguration } | { ok: false; error: string };

/** The direct child elements of a node with a local name. */
function children(node: Element, name: string): Element[] {
  return [...node.children].filter((c) => c.localName === name);
}

function child(node: Element, name: string): Element | undefined {
  return children(node, name)[0];
}

function text(node: Element | undefined): string | undefined {
  return node === undefined ? undefined : (node.textContent ?? '').trim();
}

class ParseProblem extends Error {}

function integer(node: Element | undefined, name: string): number | undefined {
  const value = text(node);
  if (value === undefined) {
    return undefined;
  }
  if (!/^-?\d+$/.test(value)) {
    throw new ParseProblem(`${name} must be an integer: "${value}"`);
  }
  return Number(value);
}

function boolean(node: Element | undefined, name: string): boolean | undefined {
  const value = text(node)?.toLowerCase();
  if (value === undefined) {
    return undefined;
  }
  if (value !== 'true' && value !== 'false') {
    throw new ParseProblem(`${name} must be true or false: "${value}"`);
  }
  return value === 'true';
}

function filterFromXml(rule: Element): LifecycleFilter | undefined {
  const legacy = text(child(rule, 'Prefix'));
  const node = child(rule, 'Filter');
  if (node === undefined) {
    return legacy !== undefined && legacy !== '' ? { prefix: legacy } : undefined;
  }
  const and = child(node, 'And');
  const scope = and ?? node;
  const filter: LifecycleFilter = {};
  const prefix = text(child(scope, 'Prefix'));
  if (prefix !== undefined && prefix !== '') {
    filter.prefix = prefix;
  }
  const tags = children(scope, 'Tag');
  if (tags.length > 0) {
    filter.tags = {};
    for (const tag of tags) {
      const key = text(child(tag, 'Key')) ?? '';
      if (key in filter.tags) {
        throw new ParseProblem(`duplicate tag key "${key}"`);
      }
      filter.tags[key] = child(tag, 'Value')?.textContent ?? '';
    }
  }
  const greater = integer(child(scope, 'ObjectSizeGreaterThan'), 'ObjectSizeGreaterThan');
  if (greater !== undefined) filter.sizeGreaterThan = greater;
  const less = integer(child(scope, 'ObjectSizeLessThan'), 'ObjectSizeLessThan');
  if (less !== undefined) filter.sizeLessThan = less;
  return Object.keys(filter).length > 0 ? filter : undefined;
}

function ruleFromElement(node: Element): LifecycleRule {
  const rule: LifecycleRule = {};
  const id = text(child(node, 'ID'));
  if (id !== undefined && id !== '') {
    rule.id = id;
  }
  const status = text(child(node, 'Status'));
  if (status !== 'Enabled' && status !== 'Disabled') {
    throw new ParseProblem(`Status must be Enabled or Disabled${id ? ` (rule ${id})` : ''}`);
  }
  rule.enabled = status === 'Enabled';
  const filter = filterFromXml(node);
  if (filter !== undefined) {
    rule.filter = filter;
  }
  const exp = child(node, 'Expiration');
  if (exp !== undefined) {
    rule.expiration = {};
    const days = integer(child(exp, 'Days'), 'Days');
    if (days !== undefined) rule.expiration.days = days;
    const date = text(child(exp, 'Date'));
    if (date !== undefined) rule.expiration.date = datePart(date);
    const marker = boolean(child(exp, 'ExpiredObjectDeleteMarker'), 'ExpiredObjectDeleteMarker');
    if (marker !== undefined) rule.expiration.expiredObjectDeleteMarker = marker;
  }
  const nc = child(node, 'NoncurrentVersionExpiration');
  if (nc !== undefined) {
    rule.noncurrentExpiration = {};
    const days = integer(child(nc, 'NoncurrentDays'), 'NoncurrentDays');
    if (days !== undefined) rule.noncurrentExpiration.noncurrentDays = days;
    const newer = integer(child(nc, 'NewerNoncurrentVersions'), 'NewerNoncurrentVersions');
    if (newer !== undefined) rule.noncurrentExpiration.newerNoncurrentVersions = newer;
  }
  const abort = child(node, 'AbortIncompleteMultipartUpload');
  if (abort !== undefined) {
    const days = integer(child(abort, 'DaysAfterInitiation'), 'DaysAfterInitiation');
    rule.abortIncompleteUpload = days === undefined ? {} : { daysAfterInitiation: days };
  }
  const transitions = children(node, 'Transition').map((t) => {
    const out: LifecycleTransition = { storageClass: text(child(t, 'StorageClass')) ?? '' };
    const days = integer(child(t, 'Days'), 'Days');
    if (days !== undefined) out.days = days;
    const date = text(child(t, 'Date'));
    if (date !== undefined) out.date = datePart(date);
    return out;
  });
  if (transitions.length > 0) {
    rule.transitions = transitions;
  }
  const ncTransitions = children(node, 'NoncurrentVersionTransition').map((t) => {
    const out: LifecycleNoncurrentTransition = {
      storageClass: text(child(t, 'StorageClass')) ?? '',
    };
    const days = integer(child(t, 'NoncurrentDays'), 'NoncurrentDays');
    if (days !== undefined) out.noncurrentDays = days;
    const newer = integer(child(t, 'NewerNoncurrentVersions'), 'NewerNoncurrentVersions');
    if (newer !== undefined) out.newerNoncurrentVersions = newer;
    return out;
  });
  if (ncTransitions.length > 0) {
    rule.noncurrentTransitions = ncTransitions;
  }
  return rule;
}

/** Parses an S3 LifecycleConfiguration document (the XML view). */
export function lifecycleFromXml(xml: string): ParseResult {
  if (xml.trim() === '') {
    return { ok: true, config: { rules: [] } };
  }
  const doc = new DOMParser().parseFromString(xml, 'application/xml');
  const failure = doc.getElementsByTagName('parsererror')[0];
  if (failure !== undefined) {
    const message = (failure.textContent ?? '').trim().split('\n')[0] ?? '';
    return { ok: false, error: message !== '' ? message : 'malformed XML' };
  }
  const root = doc.documentElement;
  if (root.localName !== 'LifecycleConfiguration') {
    return {
      ok: false,
      error: `the root element must be LifecycleConfiguration, not ${root.localName}`,
    };
  }
  try {
    return { ok: true, config: { rules: children(root, 'Rule').map(ruleFromElement) } };
  } catch (error) {
    if (error instanceof ParseProblem) {
      return { ok: false, error: error.message };
    }
    throw error;
  }
}

/* ------------------------------------------------------------------------ */
/* JSON view (the documents of the Admin API).                              */
/* ------------------------------------------------------------------------ */

/** The JSON view of the rules: the body of PUT /buckets/{bucket}/lifecycle. */
export function lifecycleToJson(config: LifecycleConfiguration): string {
  return JSON.stringify({ rules: config.rules }, null, 2);
}

type Shape = 'string' | 'number' | 'boolean' | 'object' | 'array';

const ruleShape: Record<string, Shape> = {
  id: 'string',
  enabled: 'boolean',
  filter: 'object',
  expiration: 'object',
  noncurrentExpiration: 'object',
  abortIncompleteUpload: 'object',
  transitions: 'array',
  noncurrentTransitions: 'array',
};

const nestedShapes: Record<string, Record<string, Shape>> = {
  filter: { prefix: 'string', tags: 'object', sizeGreaterThan: 'number', sizeLessThan: 'number' },
  expiration: { days: 'number', date: 'string', expiredObjectDeleteMarker: 'boolean' },
  noncurrentExpiration: { noncurrentDays: 'number', newerNoncurrentVersions: 'number' },
  abortIncompleteUpload: { daysAfterInitiation: 'number' },
};

function shapeOf(value: unknown): Shape | 'null' {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'array';
  return typeof value as Shape;
}

function checkShape(value: Record<string, unknown>, shape: Record<string, Shape>, where: string) {
  for (const [key, v] of Object.entries(value)) {
    const expected = shape[key];
    if (expected === undefined) {
      throw new ParseProblem(`${where}: unknown member "${key}"`);
    }
    if (shapeOf(v) !== expected) {
      throw new ParseProblem(`${where}: "${key}" must be a ${expected}`);
    }
  }
}

/** Parses the JSON view ({"rules": [...]}) checking the members of every rule. */
export function lifecycleFromJson(json: string): ParseResult {
  const parsed = parseJson(json);
  if (!parsed.ok) {
    const e = parsed.error;
    return {
      ok: false,
      error: `${e.message} (line ${String(e.line)}, column ${String(e.column)})`,
    };
  }
  const doc = parsed.value;
  if (shapeOf(doc) !== 'object' || shapeOf((doc as { rules?: unknown }).rules) !== 'array') {
    return { ok: false, error: 'the document must be an object with a "rules" array' };
  }
  const rules = (doc as { rules: unknown[] }).rules;
  try {
    rules.forEach((rule, i) => {
      const where = `rule ${String(i + 1)}`;
      if (shapeOf(rule) !== 'object') {
        throw new ParseProblem(`${where}: must be an object`);
      }
      const r = rule as Record<string, unknown>;
      checkShape(r, ruleShape, where);
      for (const [member, shape] of Object.entries(nestedShapes)) {
        if (r[member] !== undefined) {
          checkShape(r[member] as Record<string, unknown>, shape, `${where}.${member}`);
        }
      }
      const tags = (r.filter as { tags?: Record<string, unknown> } | undefined)?.tags;
      if (tags !== undefined && Object.values(tags).some((v) => typeof v !== 'string')) {
        throw new ParseProblem(`${where}.filter.tags: every value must be a string`);
      }
    });
  } catch (error) {
    if (error instanceof ParseProblem) {
      return { ok: false, error: error.message };
    }
    throw error;
  }
  return { ok: true, config: { rules: rules as LifecycleRule[] } };
}

/* ------------------------------------------------------------------------ */
/* Summaries of the rule list.                                              */
/* ------------------------------------------------------------------------ */

/** The parts of the scope of a rule, for the list (translated by the page). */
export interface ScopeSummary {
  prefix?: string;
  tags: string[];
  sizeGreaterThan?: number;
  sizeLessThan?: number;
}

export function scopeOf(rule: LifecycleRule): ScopeSummary {
  const f = rule.filter;
  return {
    ...(f?.prefix !== undefined && f.prefix !== '' ? { prefix: f.prefix } : {}),
    tags: Object.entries(f?.tags ?? {}).map(([k, v]) => `${k}=${v}`),
    ...(f?.sizeGreaterThan !== undefined ? { sizeGreaterThan: f.sizeGreaterThan } : {}),
    ...(f?.sizeLessThan !== undefined ? { sizeLessThan: f.sizeLessThan } : {}),
  };
}

/* ------------------------------------------------------------------------ */
/* One rule in the code views of the rule editor.                           */
/* ------------------------------------------------------------------------ */

/** The result of the parsing of the code view of one rule. */
export type RuleParseResult = { ok: true; rule: LifecycleRule } | { ok: false; error: string };

/** The JSON of one rule (a member of "rules"). */
export function ruleToJson(rule: LifecycleRule): string {
  return JSON.stringify(rule, null, 2);
}

/** Parses the JSON of one rule, checked like the rules of the JSON view. */
export function ruleFromJson(json: string): RuleParseResult {
  const parsed = parseJson(json);
  if (!parsed.ok) {
    const e = parsed.error;
    return {
      ok: false,
      error: `${e.message} (line ${String(e.line)}, column ${String(e.column)})`,
    };
  }
  const result = lifecycleFromJson(JSON.stringify({ rules: [parsed.value] }));
  if (!result.ok) {
    return {
      ok: false,
      error: result.error.replace(/^rule 1(: |\.)/, ''),
    };
  }
  const rule = result.config.rules[0];
  return rule === undefined ? { ok: false, error: 'no rule' } : { ok: true, rule };
}

/** The S3 XML of one rule (a Rule element). */
export function ruleToXml(rule: LifecycleRule): string {
  return ruleXml(rule, '').join('\n');
}

/** Parses the XML of one Rule element. */
export function ruleFromXml(xml: string): RuleParseResult {
  const result = lifecycleFromXml(
    `<LifecycleConfiguration xmlns="${s3Namespace}">${xml}</LifecycleConfiguration>`,
  );
  if (!result.ok) {
    return result;
  }
  if (result.config.rules.length !== 1) {
    return { ok: false, error: 'the document must contain exactly one Rule element' };
  }
  const rule = result.config.rules[0];
  return rule === undefined ? { ok: false, error: 'no rule' } : { ok: true, rule };
}
