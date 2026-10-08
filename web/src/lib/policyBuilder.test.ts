import { describe, expect, it } from 'vitest';

import {
  bucketResources,
  documentFromModel,
  modelFromDocument,
  newStatement,
} from './policyBuilder';

describe('policy builder model', () => {
  it('projects the statements and rebuilds the same document', () => {
    const document = {
      Version: '2012-10-17',
      Id: 'p1',
      Statement: [
        {
          Sid: 'Read',
          Effect: 'Allow',
          Action: ['s3:GetObject'],
          Resource: ['arn:aws:s3:::b/*'],
          Condition: { Bool: { 'aws:SecureTransport': 'true' } },
        },
      ],
    };
    const model = modelFromDocument(document);
    expect(model?.statements[0]).toMatchObject({
      effect: 'Allow',
      actions: ['s3:GetObject'],
      resources: ['arn:aws:s3:::b/*'],
      advanced: false,
    });
    expect(model && documentFromModel(model)).toEqual(document);
  });

  it('keeps Sid and Condition when the builder edits a statement', () => {
    const model = modelFromDocument({
      Version: '2012-10-17',
      Statement: [
        { Sid: 'S', Effect: 'Allow', Action: 's3:GetObject', Condition: { Null: { x: 'true' } } },
      ],
    });
    if (!model?.statements[0]) throw new Error('fixture');
    const edited = {
      ...model,
      statements: [{ ...model.statements[0], effect: 'Deny' as const, resources: ['*'] }],
    };
    expect(documentFromModel(edited).Statement).toEqual([
      {
        Sid: 'S',
        Effect: 'Deny',
        Action: ['s3:GetObject'],
        Condition: { Null: { x: 'true' } },
        Resource: ['*'],
      },
    ]);
  });

  it('leaves statements with NotAction or Principal untouched', () => {
    const statement = { Effect: 'Deny', NotAction: 's3:GetObject', NotResource: 'arn:aws:s3:::b' };
    const model = modelFromDocument({ Version: '2012-10-17', Statement: [statement] });
    expect(model?.statements[0]?.advanced).toBe(true);
    expect(model && documentFromModel(model).Statement).toEqual([statement]);
  });

  it('accepts a single statement object and adds new statements', () => {
    const model = modelFromDocument({
      Version: '2012-10-17',
      Statement: { Effect: 'Allow', Action: 'admin:*' },
    });
    if (!model) throw new Error('fixture');
    const next = documentFromModel({
      ...model,
      statements: [...model.statements, newStatement('Deny')],
    });
    expect(next.Statement).toEqual([
      { Effect: 'Allow', Action: ['admin:*'] },
      { Effect: 'Deny', Action: [] },
    ]);
  });

  it.each([null, 42, [], { Statement: [1] }, { Statement: [{ Action: 3 }] }])(
    'refuses a document it cannot represent: %j',
    (document) => {
      expect(modelFromDocument(document)).toBeUndefined();
    },
  );

  it('builds the ARNs of a bucket and prefix', () => {
    expect(bucketResources(' photos ', '/2026/')).toEqual([
      'arn:aws:s3:::photos',
      'arn:aws:s3:::photos/2026/*',
    ]);
    expect(bucketResources('photos')).toEqual(['arn:aws:s3:::photos', 'arn:aws:s3:::photos/*']);
    expect(bucketResources('')).toEqual([]);
  });
});
