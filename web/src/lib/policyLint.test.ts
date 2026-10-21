import { describe, expect, it } from 'vitest';

import { actionProblem, lintPolicy, resourceProblem, wildcardMatch } from './policyLint';

const doc = (statement: unknown) =>
  JSON.stringify({ Version: '2012-10-17', Statement: [statement] }, null, 2);

describe('lintPolicy', () => {
  it('accepts a valid identity policy', () => {
    expect(
      lintPolicy(
        doc({
          Sid: 'Read',
          Effect: 'Allow',
          Action: ['s3:Get*', 's3:ListBucket', 'admin:ServerInfo'],
          Resource: ['arn:aws:s3:::photos', 'arn:aws:s3:::photos/${aws:username}/*'],
          Condition: { IpAddress: { 'aws:SourceIp': '10.0.0.0/8' } },
        }),
      ),
    ).toEqual([]);
  });

  it('reports a syntax error with its position and stops there', () => {
    expect(lintPolicy('{\n  "Version": \n}')).toEqual([
      { statement: 0, element: 'JSON', line: 3, column: 1, message: "Unexpected '}'" },
    ]);
  });

  it.each([
    ['Version', { Version: '2008-10-17', Statement: [{ Effect: 'Allow', Action: 'admin:*' }] }],
    ['Statement', { Version: '2012-10-17', Statement: [] }],
    ['Effect', { Version: '2012-10-17', Statement: [{ Effect: 'Maybe', Action: 'admin:*' }] }],
    ['Action', { Version: '2012-10-17', Statement: [{ Effect: 'Allow' }] }],
    [
      'Action',
      {
        Version: '2012-10-17',
        Statement: [{ Effect: 'Allow', Action: 's3:Teleport', Resource: '*' }],
      },
    ],
    [
      'Resource',
      { Version: '2012-10-17', Statement: [{ Effect: 'Allow', Action: 's3:GetObject' }] },
    ],
    [
      'Resource',
      {
        Version: '2012-10-17',
        Statement: [{ Effect: 'Allow', Action: 's3:GetObject', Resource: 'bucket/*' }],
      },
    ],
    [
      'Principal',
      {
        Version: '2012-10-17',
        Statement: [{ Effect: 'Allow', Principal: '*', Action: 'admin:*' }],
      },
    ],
    [
      'Condition.StringFoo',
      {
        Version: '2012-10-17',
        Statement: [{ Effect: 'Allow', Action: 'admin:*', Condition: { StringFoo: { a: 'b' } } }],
      },
    ],
    [
      'Sid',
      {
        Version: '2012-10-17',
        Statement: [
          { Sid: 'x', Effect: 'Allow', Action: 'admin:*' },
          { Sid: 'x', Effect: 'Deny', Action: 'admin:*' },
        ],
      },
    ],
  ])('reports a problem of %s', (element, policy) => {
    const errors = lintPolicy(JSON.stringify(policy, null, 2));
    expect(errors.map((e) => e.element)).toContain(element);
    for (const e of errors) {
      expect(e.line).toBeGreaterThan(0);
      expect(e.column).toBeGreaterThan(0);
    }
  });

  it('locates a statement error at the element at fault', () => {
    const text = doc({ Effect: 'Allow', Action: ['s3:GetObject', 's3:Nope'], Resource: '*' });
    const [error] = lintPolicy(text);
    expect(error).toMatchObject({
      statement: 1,
      element: 'Action',
      message: 'unknown S3 action "s3:Nope"',
    });
    const line = text.split('\n')[(error?.line ?? 1) - 1] ?? '';
    expect(line.slice((error?.column ?? 1) - 1)).toMatch(/^"s3:Nope"/);
  });

  it('applies the bucket policy rules', () => {
    const policy = JSON.stringify({
      Version: '2012-10-17',
      Statement: [{ Effect: 'Allow', Action: 's3:GetObject', Resource: 'arn:aws:s3:::other/*' }],
    });
    const elements = lintPolicy(policy, { kind: 'bucket', bucket: 'photos' }).map((e) => e.element);
    expect(elements).toEqual(expect.arrayContaining(['Principal', 'Resource']));
  });
});

describe('policy element checks', () => {
  it.each([
    ['*', undefined],
    ['s3:*', undefined],
    ['s3:Get*', undefined],
    ['S3:GETOBJECT', undefined],
    ['admin:Anything', undefined],
    ['sts:AssumeRole', undefined],
    ['GetObject', 'invalid action "GetObject" (expected <service>:<action>)'],
    ['ec2:Run', 'invalid action "ec2:Run": unknown service "ec2" (s3, nexo, admin, sts, kms)'],
    ['s3:Zap*', 'unknown S3 action "s3:Zap*"'],
  ])('action %s', (action, problem) => {
    expect(actionProblem(action)).toBe(problem);
  });

  it.each([
    ['*', true],
    ['arn:aws:s3:::b', true],
    ['arn:aws:s3:::', false],
    ['b/*', false],
  ])('resource %s valid=%s', (resource, valid) => {
    expect(resourceProblem(resource) === undefined).toBe(valid);
  });

  it.each([
    ['s3:Get*', 's3:GetObject', true],
    ['s3:?etObject', 's3:GetObject', true],
    ['arn:aws:s3:::b/*', 'arn:aws:s3:::b/x/y', true],
    ['arn:aws:s3:::b/*', 'arn:aws:s3:::c/x', false],
    ['a.b', 'axb', false],
  ])('wildcard %s ~ %s = %s', (pattern, value, expected) => {
    expect(wildcardMatch(pattern, value)).toBe(expected);
  });
});
