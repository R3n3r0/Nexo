/**
 * A strict JSON parser that keeps the position of every value. The policy
 * editor uses it to point at syntax errors (JSON.parse messages carry no
 * reliable position across engines) and to locate semantic problems of a
 * document by JSON pointer ("/Statement/0/Effect").
 */

/** 1-based line and column of an offset in a text. */
export interface TextPosition {
  /** 0-based offset in the text. */
  offset: number;
  line: number;
  column: number;
}

export interface JsonSyntaxError extends TextPosition {
  message: string;
}

export type JsonParseResult =
  | {
      ok: true;
      value: unknown;
      /** Start of every value, keyed by JSON pointer ("" is the root). */
      positions: ReadonlyMap<string, TextPosition>;
    }
  | { ok: false; error: JsonSyntaxError };

/** Computes the 1-based line and column of an offset. */
export function positionAt(text: string, offset: number): TextPosition {
  const clamped = Math.max(0, Math.min(offset, text.length));
  let line = 1;
  let lineStart = 0;
  for (let i = 0; i < clamped; i += 1) {
    if (text.charCodeAt(i) === 10) {
      line += 1;
      lineStart = i + 1;
    }
  }
  return { offset: clamped, line, column: clamped - lineStart + 1 };
}

/** Escapes a key for use in a JSON pointer (RFC 6901). */
export function pointerSegment(key: string | number): string {
  return String(key).replace(/~/g, '~0').replace(/\//g, '~1');
}

class SyntaxFailure extends Error {
  constructor(
    message: string,
    readonly offset: number,
  ) {
    super(message);
  }
}

/** Parses JSON text, returning either the value with positions or the first syntax error. */
export function parseJson(text: string): JsonParseResult {
  const positions = new Map<string, TextPosition>();
  let i = 0;

  const fail = (message: string, at = i): never => {
    throw new SyntaxFailure(message, at);
  };
  const skipWhitespace = () => {
    while (i < text.length) {
      const c = text[i];
      if (c === ' ' || c === '\t' || c === '\n' || c === '\r') {
        i += 1;
      } else {
        break;
      }
    }
  };
  const describe = (at: number) => (at >= text.length ? 'end of input' : `'${text[at] ?? ''}'`);

  const parseString = (): string => {
    const start = i;
    i += 1; // opening quote
    let out = '';
    while (i < text.length) {
      const c = text[i] ?? '';
      if (c === '"') {
        i += 1;
        return out;
      }
      if (c === '\\') {
        const next = text[i + 1];
        const simple: Record<string, string> = {
          '"': '"',
          '\\': '\\',
          '/': '/',
          b: '\b',
          f: '\f',
          n: '\n',
          r: '\r',
          t: '\t',
        };
        if (next !== undefined && next in simple) {
          out += simple[next] ?? '';
          i += 2;
          continue;
        }
        if (next === 'u') {
          const hex = text.slice(i + 2, i + 6);
          if (!/^[0-9a-fA-F]{4}$/.test(hex)) {
            fail('Invalid unicode escape', i);
          }
          out += String.fromCharCode(parseInt(hex, 16));
          i += 6;
          continue;
        }
        fail('Invalid escape sequence', i);
      }
      if (c.charCodeAt(0) < 0x20) {
        fail('Control character in string', i);
      }
      out += c;
      i += 1;
    }
    return fail('Unterminated string', start);
  };

  const parseNumber = (): number => {
    const match = /^-?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?/.exec(text.slice(i));
    if (!match) {
      return fail(`Unexpected ${describe(i)}`);
    }
    i += match[0].length;
    return Number(match[0]);
  };

  const parseValue = (pointer: string): unknown => {
    skipWhitespace();
    positions.set(pointer, positionAt(text, i));
    const c = text[i];
    if (c === '{') {
      return parseObject(pointer);
    }
    if (c === '[') {
      return parseArray(pointer);
    }
    if (c === '"') {
      return parseString();
    }
    if (c === '-' || (c !== undefined && c >= '0' && c <= '9')) {
      return parseNumber();
    }
    for (const [word, value] of [
      ['true', true],
      ['false', false],
      ['null', null],
    ] as const) {
      if (text.startsWith(word, i)) {
        i += word.length;
        return value;
      }
    }
    return fail(i >= text.length ? 'Unexpected end of input' : `Unexpected ${describe(i)}`);
  };

  const parseObject = (pointer: string): Record<string, unknown> => {
    const out: Record<string, unknown> = {};
    i += 1;
    skipWhitespace();
    if (text[i] === '}') {
      i += 1;
      return out;
    }
    for (;;) {
      skipWhitespace();
      if (text[i] !== '"') {
        fail(`Expected a property name but found ${describe(i)}`);
      }
      const keyStart = i;
      const key = parseString();
      if (Object.hasOwn(out, key)) {
        fail(`Duplicate property "${key}"`, keyStart);
      }
      skipWhitespace();
      if (text[i] !== ':') {
        fail(`Expected ':' but found ${describe(i)}`);
      }
      i += 1;
      out[key] = parseValue(`${pointer}/${pointerSegment(key)}`);
      skipWhitespace();
      if (text[i] === ',') {
        i += 1;
        skipWhitespace();
        if (text[i] === '}') {
          fail('Trailing comma');
        }
        continue;
      }
      if (text[i] === '}') {
        i += 1;
        return out;
      }
      fail(`Expected ',' or '}' but found ${describe(i)}`);
    }
  };

  const parseArray = (pointer: string): unknown[] => {
    const out: unknown[] = [];
    i += 1;
    skipWhitespace();
    if (text[i] === ']') {
      i += 1;
      return out;
    }
    for (;;) {
      out.push(parseValue(`${pointer}/${out.length}`));
      skipWhitespace();
      if (text[i] === ',') {
        i += 1;
        skipWhitespace();
        if (text[i] === ']') {
          fail('Trailing comma');
        }
        continue;
      }
      if (text[i] === ']') {
        i += 1;
        return out;
      }
      fail(`Expected ',' or ']' but found ${describe(i)}`);
    }
  };

  try {
    skipWhitespace();
    if (i >= text.length) {
      fail('The document is empty');
    }
    const value = parseValue('');
    skipWhitespace();
    if (i < text.length) {
      fail(`Unexpected ${describe(i)} after the document`);
    }
    return { ok: true, value, positions };
  } catch (cause) {
    if (cause instanceof SyntaxFailure) {
      return { ok: false, error: { message: cause.message, ...positionAt(text, cause.offset) } };
    }
    throw cause;
  }
}

/** Pretty prints a value the way the editors show documents (2 spaces). */
export function formatJson(value: unknown): string {
  return JSON.stringify(value, null, 2);
}
