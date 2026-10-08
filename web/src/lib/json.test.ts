import { describe, expect, it } from 'vitest';

import { parseJson, pointerSegment, positionAt } from './json';

describe('positionAt', () => {
  it.each([
    ['abc', 0, 1, 1],
    ['abc', 2, 1, 3],
    ['a\nbc', 2, 2, 1],
    ['a\nbc', 3, 2, 2],
    ['a\n\nb', 3, 3, 1],
  ])('%j at %i is line %i column %i', (text, offset, line, column) => {
    expect(positionAt(text, offset)).toMatchObject({ line, column });
  });
});

describe('parseJson', () => {
  it('parses a document and records the position of every value', () => {
    const text =
      '{\n  "Version": "2012-10-17",\n  "Statement": [\n    { "Effect": "Allow" }\n  ]\n}';
    const result = parseJson(text);
    if (!result.ok) throw new Error(result.error.message);
    expect(result.value).toEqual({ Version: '2012-10-17', Statement: [{ Effect: 'Allow' }] });
    expect(result.positions.get('/Version')).toMatchObject({ line: 2, column: 14 });
    expect(result.positions.get('/Statement/0/Effect')).toMatchObject({ line: 4, column: 17 });
  });

  it('decodes strings, numbers and literals like JSON.parse', () => {
    const text = '{"s":"a\\n\\u00e8\\"","n":-1.5e2,"t":true,"f":false,"z":null,"a":[]}';
    const result = parseJson(text);
    expect(result.ok && result.value).toEqual(JSON.parse(text));
  });

  it.each([
    ['', 'The document is empty', 1, 1],
    ['{', 'Expected a property name but found end of input', 1, 2],
    ['{"a": }', "Unexpected '}'", 1, 7],
    ['{"a": 1,}', 'Trailing comma', 1, 9],
    ['[1,]', 'Trailing comma', 1, 4],
    ['{"a": 1 "b": 2}', "Expected ',' or '}' but found '\"'", 1, 9],
    ['{\n  "a": 1,\n  "a": 2\n}', 'Duplicate property "a"', 3, 3],
    ['"abc', 'Unterminated string', 1, 1],
    ['{} x', "Unexpected 'x' after the document", 1, 4],
    ['{"a": tru}', "Unexpected 't'", 1, 7],
  ])('locates the error of %j', (text, message, line, column) => {
    const result = parseJson(text);
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.error).toMatchObject({ message, line, column });
  });
});

describe('pointerSegment', () => {
  it('escapes ~ and / as in RFC 6901', () => {
    expect(pointerSegment('a/b~c')).toBe('a~1b~0c');
    expect(pointerSegment(3)).toBe('3');
  });
});
