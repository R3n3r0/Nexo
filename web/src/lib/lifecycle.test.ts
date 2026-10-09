import { describe, expect, it } from 'vitest';

import type { LifecycleRule } from '@/services';

import {
  emptyRuleForm,
  formToRule,
  isMidnightDate,
  isValidRuleForm,
  lifecycleFromJson,
  lifecycleFromXml,
  lifecycleToJson,
  lifecycleToXml,
  ruleFromJson,
  ruleFromXml,
  ruleToForm,
  ruleToJson,
  ruleToXml,
  validateRule,
  validateRuleForm,
  type LifecycleRuleForm,
} from './lifecycle';

/** TD-U-08: form <-> rules <-> XML/JSON conversions and validations. */
describe('lifecycle rules', () => {
  const full: LifecycleRule = {
    id: 'logs',
    enabled: true,
    filter: { prefix: 'logs/', tags: { team: 'ops' }, sizeGreaterThan: 1024, sizeLessThan: 4096 },
    expiration: { days: 30 },
    noncurrentExpiration: { noncurrentDays: 7, newerNoncurrentVersions: 3 },
  };
  const cases: { name: string; rule: LifecycleRule }[] = [
    { name: 'every filter and two actions', rule: full },
    {
      name: 'prefix and date',
      rule: {
        id: 'tmp',
        enabled: false,
        filter: { prefix: 'tmp/' },
        expiration: { date: '2030-01-01' },
      },
    },
    {
      name: 'whole bucket, delete markers and uploads',
      rule: {
        id: 'cleanup',
        enabled: true,
        expiration: { expiredObjectDeleteMarker: true },
        abortIncompleteUpload: { daysAfterInitiation: 7 },
      },
    },
    {
      name: 'one tag only',
      rule: {
        id: 'tagged',
        enabled: true,
        filter: { tags: { a: 'b & c' } },
        expiration: { days: 1 },
      },
    },
    {
      name: 'size only, noncurrent without newer versions',
      rule: {
        id: 'big',
        enabled: true,
        filter: { sizeGreaterThan: 0 },
        noncurrentExpiration: { noncurrentDays: 1 },
      },
    },
  ];

  it.each(cases)('round-trips $name through the form', ({ rule }) => {
    expect(formToRule(ruleToForm(rule))).toEqual(rule);
  });

  it.each(cases)('round-trips $name through the XML document', ({ rule }) => {
    const xml = lifecycleToXml({ rules: [rule] });
    const parsed = lifecycleFromXml(xml);
    expect(parsed).toEqual({ ok: true, config: { rules: [rule] } });
  });

  it.each(cases)('round-trips $name through the JSON view', ({ rule }) => {
    expect(lifecycleFromJson(lifecycleToJson({ rules: [rule] }))).toEqual({
      ok: true,
      config: { rules: [rule] },
    });
  });

  it.each(cases)('round-trips $name through the code views of one rule', ({ rule }) => {
    expect(ruleFromJson(ruleToJson(rule))).toEqual({ ok: true, rule });
    expect(ruleFromXml(ruleToXml(rule))).toEqual({ ok: true, rule });
  });

  it('writes the S3 document: And for several criteria, midnight dates, status', () => {
    const xml = lifecycleToXml({
      rules: [full, { id: 'd', enabled: false, expiration: { date: '2030-01-01' } }],
    });
    expect(xml).toContain(
      '<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">',
    );
    expect(xml).toContain('<And>');
    expect(xml).toContain('<Prefix>logs/</Prefix>');
    expect(xml).toContain('<ObjectSizeGreaterThan>1024</ObjectSizeGreaterThan>');
    expect(xml).toContain('<NewerNoncurrentVersions>3</NewerNoncurrentVersions>');
    expect(xml).toContain('<Date>2030-01-01T00:00:00Z</Date>');
    expect(xml).toContain('<Status>Disabled</Status>');
    expect(xml).toContain('<Filter></Filter>');
  });

  it('reads the legacy Prefix of a rule and the S3 XML of other tools', () => {
    const parsed = lifecycleFromXml(`<?xml version="1.0" encoding="UTF-8"?>
      <LifecycleConfiguration><Rule><ID>old</ID><Prefix>a/</Prefix><Status>Enabled</Status>
      <Expiration><Days>5</Days></Expiration></Rule></LifecycleConfiguration>`);
    expect(parsed).toEqual({
      ok: true,
      config: {
        rules: [{ id: 'old', enabled: true, filter: { prefix: 'a/' }, expiration: { days: 5 } }],
      },
    });
  });

  it.each([
    ['malformed XML', '<LifecycleConfiguration><Rule>', /./],
    ['another root', '<CORSConfiguration/>', /LifecycleConfiguration/],
    [
      'a bad status',
      '<LifecycleConfiguration><Rule><Status>On</Status></Rule></LifecycleConfiguration>',
      /Status/,
    ],
    [
      'a non-integer number',
      '<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>x</Days></Expiration></Rule></LifecycleConfiguration>',
      /Days must be an integer/,
    ],
  ])('rejects %s', (_, xml, message) => {
    const parsed = lifecycleFromXml(xml);
    expect(parsed.ok).toBe(false);
    if (!parsed.ok) expect(parsed.error).toMatch(message);
  });

  it.each([
    ['invalid JSON', '{"rules": [', /line 1/],
    ['no rules', '{}', /"rules" array/],
    ['an unknown member', '{"rules":[{"days":1}]}', /rule 1: unknown member "days"/],
    [
      'a wrong type',
      '{"rules":[{"expiration":{"days":"1"}}]}',
      /rule 1.expiration: "days" must be a number/,
    ],
    ['a non-string tag', '{"rules":[{"filter":{"tags":{"a":1}}}]}', /tags/],
  ])('rejects a JSON view with %s', (_, json, message) => {
    const parsed = lifecycleFromJson(json);
    expect(parsed.ok).toBe(false);
    if (!parsed.ok) expect(parsed.error).toMatch(message);
  });

  it('rejects code views of one rule that are not one rule', () => {
    expect(
      ruleFromXml('<Rule><Status>Enabled</Status></Rule><Rule><Status>Enabled</Status></Rule>').ok,
    ).toBe(false);
    expect(ruleFromJson('[]').ok).toBe(false);
    expect(ruleFromJson('{"id": 1}')).toEqual({ ok: false, error: '"id" must be a string' });
  });
});

describe('validateRuleForm', () => {
  const form = (patch: Partial<LifecycleRuleForm>): LifecycleRuleForm => ({
    ...emptyRuleForm(),
    ...patch,
  });

  it('accepts the default rule', () => {
    expect(isValidRuleForm(validateRuleForm(emptyRuleForm()))).toBe(true);
  });

  it.each<[string, Partial<LifecycleRuleForm>, Record<string, string>]>([
    ['zero days', { expirationDays: 0 }, { expirationDays: 'positive' }],
    ['empty days', { expirationDays: '' }, { expirationDays: 'positive' }],
    ['fractional days', { expirationDays: 1.5 }, { expirationDays: 'positive' }],
    [
      'a date not at midnight',
      { expiration: 'date', expirationDate: '2030-02-30' },
      { expirationDate: 'date' },
    ],
    ['no action', { expiration: 'none' }, { rule: 'noAction' }],
    ['noncurrent days', { noncurrent: true, noncurrentDays: 0 }, { noncurrentDays: 'positive' }],
    [
      'newer versions',
      { noncurrent: true, newerNoncurrentVersions: 101 },
      { newerNoncurrentVersions: 'newerRange' },
    ],
    ['abort days', { abortUploads: true, abortDays: -1 }, { abortDays: 'positive' }],
    ['an empty tag key', { tags: [{ key: '', value: 'x' }] }, { tags: 'tagKey' }],
    [
      'duplicate tag keys',
      {
        tags: [
          { key: 'a', value: '1' },
          { key: 'a', value: '2' },
        ],
      },
      { tags: 'tagDuplicate' },
    ],
    [
      'an empty size range',
      { sizeGreaterThan: 10, sizeLessThan: 10 },
      { sizeLessThan: 'sizeRange' },
    ],
    ['a negative size', { sizeGreaterThan: -1 }, { sizeGreaterThan: 'sizeNegative' }],
    [
      'abort with a tag filter',
      { abortUploads: true, tags: [{ key: 'a', value: 'b' }] },
      { rule: 'abortWithFilter' },
    ],
    [
      'abort with a size filter',
      { abortUploads: true, sizeLessThan: 5 },
      { rule: 'abortWithFilter' },
    ],
    [
      'delete markers with tags',
      { expiration: 'deleteMarker', tags: [{ key: 'a', value: 'b' }] },
      { rule: 'deleteMarkerWithTags' },
    ],
    ['a too long id', { id: 'x'.repeat(256) }, { id: 'idTooLong' }],
  ])('reports %s', (_, patch, expected) => {
    expect(validateRuleForm(form(patch))).toEqual(expected);
  });

  it('reports an id used by another rule', () => {
    expect(validateRuleForm(form({ id: 'a' }), ['a'])).toEqual({ id: 'idDuplicate' });
  });

  it('validates the rules of the API like the server', () => {
    expect(validateRule({ expiration: { days: 1, expiredObjectDeleteMarker: true } })).toContain(
      'expirationOne',
    );
    expect(validateRule({ noncurrentExpiration: { newerNoncurrentVersions: 2 } })).toContain(
      'positive',
    );
    expect(validateRule({ expiration: { days: 1 } })).toEqual([]);
  });

  it('knows a midnight date', () => {
    expect(isMidnightDate('2030-01-01')).toBe(true);
    expect(isMidnightDate('2030-01-01T00:00:00Z')).toBe(true);
    expect(isMidnightDate('2030-01-01T10:00:00Z')).toBe(false);
    expect(isMidnightDate('2030-13-01')).toBe(false);
  });
});
