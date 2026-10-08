/**
 * Model of the visual policy builder (D-POL-02). The builder edits Effect,
 * Action and Resource of each statement and keeps every other element (Sid,
 * Condition...) untouched, so that switching between the builder and the
 * JSON editor never loses information: the JSON text stays the single
 * source of truth and the builder is a projection of it.
 */
import { s3Arn, s3ArnPrefix } from './policyCatalog';
import { policyVersion } from './policyLint';

export type Effect = 'Allow' | 'Deny';

export interface BuilderStatement {
  effect: Effect;
  actions: string[];
  resources: string[];
  /** The statement as found in the document (elements the builder keeps). */
  original: Record<string, unknown>;
  /**
   * The statement uses elements the builder cannot edit (NotAction,
   * NotResource, Principal): it is shown read-only.
   */
  advanced: boolean;
}

export interface BuilderModel {
  /** Top-level elements other than Statement (Version, Id). */
  base: Record<string, unknown>;
  statements: BuilderStatement[];
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

const toList = (value: unknown): string[] | undefined => {
  if (value === undefined) return [];
  if (typeof value === 'string') return [value];
  if (Array.isArray(value) && value.every((v): v is string => typeof v === 'string')) return value;
  return undefined;
};

/**
 * Projects a parsed document on the builder model; undefined when the
 * document does not have the shape of a policy (the builder then asks to
 * fix the JSON first).
 */
export function modelFromDocument(document: unknown): BuilderModel | undefined {
  if (!isRecord(document)) return undefined;
  const raw = document.Statement;
  const list = raw === undefined ? [] : Array.isArray(raw) ? raw : [raw];
  const statements: BuilderStatement[] = [];
  for (const item of list) {
    if (!isRecord(item)) return undefined;
    const actions = toList(item.Action);
    const resources = toList(item.Resource);
    if (actions === undefined || resources === undefined) return undefined;
    const advanced =
      item.NotAction !== undefined ||
      item.NotResource !== undefined ||
      item.Principal !== undefined ||
      item.NotPrincipal !== undefined ||
      (item.Effect !== 'Allow' && item.Effect !== 'Deny');
    statements.push({
      effect: item.Effect === 'Deny' ? 'Deny' : 'Allow',
      actions,
      resources,
      original: item,
      advanced,
    });
  }
  const { Statement: _statement, ...base } = document;
  return { base, statements };
}

/** Rebuilds the document of a model. */
export function documentFromModel(model: BuilderModel): Record<string, unknown> {
  const statements = model.statements.map((s) => {
    if (s.advanced) return s.original;
    const next: Record<string, unknown> = { ...s.original, Effect: s.effect, Action: s.actions };
    if (s.resources.length > 0) {
      next.Resource = s.resources;
    } else {
      delete next.Resource;
    }
    return next;
  });
  return { Version: policyVersion, ...model.base, Statement: statements };
}

/** A new, empty statement. */
export function newStatement(effect: Effect = 'Allow'): BuilderStatement {
  return { effect, actions: [], resources: [], original: {}, advanced: false };
}

/** The document of a new policy. */
export function emptyPolicyDocument(): Record<string, unknown> {
  return {
    Version: policyVersion,
    Statement: [{ Effect: 'Allow', Action: ['s3:GetObject'], Resource: [`${s3ArnPrefix}*`] }],
  };
}

/**
 * ARNs granting access to a bucket and to the objects under a prefix:
 * the bucket itself (ListBucket...) and "bucket/prefix*" (objects).
 */
export function bucketResources(bucket: string, prefix = ''): string[] {
  const name = bucket.trim();
  if (name === '') return [];
  return [s3Arn(name), s3Arn(name, prefix.trim().replace(/^\/+/, ''))];
}

/** Ready-made action sets of the builder. */
export const actionPresets = {
  readOnly: ['s3:GetBucketLocation', 's3:ListBucket', 's3:GetObject'],
  readWrite: [
    's3:GetBucketLocation',
    's3:ListBucket',
    's3:GetObject',
    's3:PutObject',
    's3:DeleteObject',
    's3:AbortMultipartUpload',
    's3:ListMultipartUploadParts',
  ],
  writeOnly: ['s3:PutObject', 's3:AbortMultipartUpload', 's3:ListMultipartUploadParts'],
  fullS3: ['s3:*'],
} as const;

export type ActionPreset = keyof typeof actionPresets;
