import { describe, expect, it } from 'vitest';

import { parseYaml, toYaml, type YamlValue } from './yaml';

function parsed(text: string): YamlValue {
  const result = parseYaml(text);
  if (!result.ok) {
    throw new Error(
      `${String(result.error.line)}:${String(result.error.column)} ${result.error.message}`,
    );
  }
  return result.value;
}

/** The access rules example of docs/10 §10.1, as written there. */
const docsExample = `bucket: media
rules:
  - id: public-images
    priority: 10                 # evaluation order (lower = first)
    effect: allow                # allow | deny
    principals: ["*"]            # "*" = anonymous/everyone
    actions: [read]
    match:
      type: regex                # prefix | glob | regex
      pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png|webp)$'
      caseInsensitive: true
    conditions:                  # optional
      sourceIp: ["10.0.0.0/8", "2001:db8::/32"]
      notSourceIp: []
      referer: ["https://*.example.com/*"]
      host: ["cdn.example.com"]  # domain the request arrived on
      secureTransport: true
      timeWindow: { from: "2026-01-01T00:00:00Z", to: "2026-12-31T23:59:59Z" }
      methods: [GET, HEAD]
      headers: { "X-Origin-Secret": "\${secret:proxy-token}" }
  - id: block-private
    priority: 5
    effect: deny
    principals: ["*"]
    actions: [all]
    match: { type: glob, pattern: "**/private/**" }
`;

describe('parseYaml', () => {
  it('reads the access rules example of the documentation', () => {
    expect(parsed(docsExample)).toEqual({
      bucket: 'media',
      rules: [
        {
          id: 'public-images',
          priority: 10,
          effect: 'allow',
          principals: ['*'],
          actions: ['read'],
          match: {
            type: 'regex',
            pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png|webp)$',
            caseInsensitive: true,
          },
          conditions: {
            sourceIp: ['10.0.0.0/8', '2001:db8::/32'],
            notSourceIp: [],
            referer: ['https://*.example.com/*'],
            host: ['cdn.example.com'],
            secureTransport: true,
            timeWindow: { from: '2026-01-01T00:00:00Z', to: '2026-12-31T23:59:59Z' },
            methods: ['GET', 'HEAD'],
            headers: { 'X-Origin-Secret': '${secret:proxy-token}' },
          },
        },
        {
          id: 'block-private',
          priority: 5,
          effect: 'deny',
          principals: ['*'],
          actions: ['all'],
          match: { type: 'glob', pattern: '**/private/**' },
        },
      ],
    });
  });

  it.each([
    [
      'plain scalars',
      'a: 1\nb: -2.5\nc: true\nd: ~\ne: text with spaces',
      { a: 1, b: -2.5, c: true, d: null, e: 'text with spaces' },
    ],
    [
      'quoted scalars keep their type',
      `a: "1"\nb: 'true'\nc: 'it''s'\nd: "tab\\there"`,
      { a: '1', b: 'true', c: "it's", d: 'tab\there' },
    ],
    [
      'sequences at the indentation of their key',
      'list:\n- a\n- b\nnext: 1',
      { list: ['a', 'b'], next: 1 },
    ],
    ['nested sequences', '- - a\n  - b\n- c', [['a', 'b'], 'c']],
    ['empty values', 'a:\nb: []\nc: {}', { a: null, b: [], c: {} }],
    [
      'multi-line flow collections',
      'a: [x,\n  y,\n  z]\nb: {k: v,\n  l: w}',
      { a: ['x', 'y', 'z'], b: { k: 'v', l: 'w' } },
    ],
    [
      'literal block scalars',
      'text: |\n  line 1\n  line 2\nnext: x',
      { text: 'line 1\nline 2\n', next: 'x' },
    ],
    ['folded block scalars', 'text: >-\n  a\n  b\n\n  c\n', { text: 'a b\nc' }],
    ['the document marker', '---\na: 1\n...\n', { a: 1 }],
    ['a comment with a hash in a value', 'a: x#y # comment', { a: 'x#y' }],
    ['URLs as plain scalars', '- https://example.com/a:b', ['https://example.com/a:b']],
    [
      'a bare list',
      '- id: r1\n  effect: deny\n- id: r2\n',
      [{ id: 'r1', effect: 'deny' }, { id: 'r2' }],
    ],
  ])('reads %s', (_, text, want) => {
    expect(parsed(text)).toEqual(want);
  });

  it.each([
    ['a duplicate key', 'a: 1\na: 2', 2, 'duplicate key "a"'],
    ['a bad indentation', 'a:\n  b: 1\n    c: 2', 3, 'unexpected indentation'],
    ['an unterminated flow sequence', 'a: [x, y', 1, 'unterminated flow sequence: missing ]'],
    ['an unterminated string', "a: 'x", 1, 'unterminated single-quoted string'],
    ['an alias', 'a: *ref', 1, 'anchors and aliases are not supported'],
    ['two documents', 'a: 1\n---\nb: 2', 2, 'only one YAML document is supported'],
    ['a mapping value in a scalar', 'a: b: c', 1, 'a mapping value is not allowed here'],
    ['a sequence item in a mapping', 'a: 1\n- b', 2, 'a sequence item where a key is expected'],
  ])('locates %s', (_, text, line, message) => {
    const result = parseYaml(text);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.error.line).toBe(line);
      expect(result.error.message).toBe(message);
    }
  });
});

describe('toYaml', () => {
  it('writes block mappings, flow sequences of scalars and quotes what needs it', () => {
    expect(
      toYaml({
        rules: [
          {
            id: 'r1',
            principals: ['*', 'user:mario'],
            match: { type: 'glob', pattern: '**/private/**' },
            conditions: { headers: { 'X-Origin-Secret': '${secret:a}' }, methods: [] },
            note: "it's: yes",
            flag: 'true',
            n: 10,
          },
        ],
      }),
    ).toBe(
      [
        'rules:',
        '  - id: r1',
        "    principals: ['*', user:mario]",
        '    match:',
        '      type: glob',
        "      pattern: '**/private/**'",
        '    conditions:',
        '      headers:',
        "        X-Origin-Secret: '${secret:a}'",
        '      methods: []',
        "    note: 'it''s: yes'",
        "    flag: 'true'",
        '    n: 10',
        '',
      ].join('\n'),
    );
  });

  it.each([
    { a: 'line\nbreak', b: '', c: ' padded ', d: '- dash', e: '#hash', f: '1.5', g: null },
    [{ k: [1, 2, { x: 'y' }] }, [], {}, 'plain'],
    { pattern: '^img/[0-9]{4}/.*\\.(jpe?g|png)$', glob: '{img,video}/*', prefix: 'a b/c' },
    { long: Array.from({ length: 20 }, (_, i) => `item-number-${String(i)}`) },
  ])('round-trips %#', (value) => {
    expect(parsed(toYaml(value))).toEqual(value);
  });

  it('drops undefined members like JSON', () => {
    expect(toYaml({ a: 1, b: undefined })).toBe('a: 1\n');
  });
});
