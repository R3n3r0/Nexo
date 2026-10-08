/**
 * JSON schema of AWS policy documents for the Monaco editor: validation of
 * the structure and autocompletion of actions, condition operators and
 * condition keys (D-POL-02). Wildcard actions and resources stay valid
 * through the pattern branches; the enum branches only feed completion.
 */
import { conditionKeys, conditionOperatorNames, policyActions, s3ArnPrefix } from './policyCatalog';
import type { PolicyKind } from './policyLint';
import { policyVersion } from './policyLint';

const stringOrStrings = (item: object) => ({
  anyOf: [item, { type: 'array', items: item, minItems: 1 }],
});

const actionItem = {
  type: 'string',
  anyOf: [{ enum: [...policyActions] }, { pattern: '^(\\*|[A-Za-z0-9]+:[A-Za-z0-9*?_-]+)$' }],
};

const resourceItem = {
  type: 'string',
  anyOf: [
    { enum: ['*', `${s3ArnPrefix}*`, `${s3ArnPrefix}\${aws:username}/*`] },
    { pattern: `^(\\*|arn:aws:s3:::.+)$` },
  ],
};

const conditionValue = stringOrStrings({ type: ['string', 'boolean', 'number'] });

const conditionBlock = {
  type: 'object',
  minProperties: 1,
  properties: Object.fromEntries(conditionKeys.map((key) => [key, conditionValue])),
  additionalProperties: conditionValue,
};

const operatorPattern = `^(ForAllValues:|ForAnyValue:)?(${conditionOperatorNames.join('|')})(IfExists)?$`;

const principal = {
  anyOf: [
    { const: '*' },
    {
      type: 'object',
      properties: {
        AWS: stringOrStrings({ type: 'string' }),
        Service: stringOrStrings({ type: 'string' }),
        Federated: stringOrStrings({ type: 'string' }),
      },
      additionalProperties: false,
    },
  ],
};

/** Builds the schema of an identity (IAM, inline) or bucket policy. */
export function buildPolicySchema(kind: PolicyKind = 'identity'): object {
  const statementProperties: Record<string, unknown> = {
    Sid: { type: 'string', description: 'Statement identifier' },
    Effect: { enum: ['Allow', 'Deny'] },
    Action: stringOrStrings(actionItem),
    NotAction: stringOrStrings(actionItem),
    Resource: stringOrStrings(resourceItem),
    NotResource: stringOrStrings(resourceItem),
    Condition: {
      type: 'object',
      properties: Object.fromEntries(
        conditionOperatorNames.map((operator) => [operator, conditionBlock]),
      ),
      patternProperties: { [operatorPattern]: conditionBlock },
      additionalProperties: false,
    },
  };
  if (kind === 'bucket') {
    statementProperties.Principal = principal;
    statementProperties.NotPrincipal = principal;
  }
  const statement = {
    type: 'object',
    required: ['Effect'],
    properties: statementProperties,
    additionalProperties: false,
  };
  return {
    $schema: 'http://json-schema.org/draft-07/schema#',
    title: kind === 'bucket' ? 'S3 bucket policy' : 'IAM policy',
    type: 'object',
    required: ['Version', 'Statement'],
    properties: {
      Version: { enum: [policyVersion] },
      Id: { type: 'string' },
      Statement: { anyOf: [statement, { type: 'array', items: statement, minItems: 1 }] },
    },
    additionalProperties: false,
  };
}

/** Schema URIs registered in the editor. */
export const policySchemaUris = {
  identity: 'nexo://schemas/iam-policy.json',
  bucket: 'nexo://schemas/bucket-policy.json',
} as const;
