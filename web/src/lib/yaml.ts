/**
 * A small YAML reader and writer for the documents the console shows as
 * YAML (access rules, distributions: `nexoctl access export`, `nexoctl
 * dist export`). It covers the block and flow styles of mappings and
 * sequences, plain, single- and double-quoted scalars, literal and folded
 * block scalars and comments: what such documents use. Anchors, aliases,
 * tags and multi-document streams are refused with a located error.
 */

export type YamlValue =
  null | boolean | number | string | YamlValue[] | { [key: string]: YamlValue };

/** A located parse error (1-based line and column). */
export interface YamlError {
  line: number;
  column: number;
  message: string;
}

export type YamlParseResult = { ok: true; value: YamlValue } | { ok: false; error: YamlError };

/* ------------------------------------------------------------------------ */
/* Writer                                                                   */
/* ------------------------------------------------------------------------ */

const reserved =
  /^(?:~|null|Null|NULL|true|True|TRUE|false|False|FALSE|y|Y|n|N|yes|Yes|no|No|on|On|off|Off)$/;
const numberLike = /^[-+]?(?:\.?\d|0x|0o|\.inf|\.nan)/i;

/** True when a string can be written without quotes. */
function plainSafe(s: string): boolean {
  if (s === '' || s.trim() !== s) return false;
  if (reserved.test(s) || numberLike.test(s)) return false;
  // Indicators that cannot start a plain scalar.
  if (/^[-?:,[\]{}#&*!|>'"%@`]/.test(s)) return false;
  // ": " and " #" change the meaning; flow indicators break flow collections.
  if (s.includes(': ') || s.includes(' #') || s.endsWith(':')) return false;
  if (/[,[\]{}]/.test(s)) return false;
  // Control characters and line breaks need the double-quoted style.
  // eslint-disable-next-line no-control-regex -- control characters are what is checked
  if (/[\u0000-\u001f\u007f]/.test(s)) return false;
  return true;
}

function quoteString(s: string): string {
  // eslint-disable-next-line no-control-regex -- control characters are what is checked
  if (/[\u0000-\u001f\u007f]/.test(s)) {
    return JSON.stringify(s);
  }
  return `'${s.replaceAll("'", "''")}'`;
}

function scalar(value: null | boolean | number | string): string {
  if (value === null) return 'null';
  if (typeof value === 'boolean') return value ? 'true' : 'false';
  if (typeof value === 'number') {
    if (Number.isNaN(value)) return '.nan';
    if (!Number.isFinite(value)) return value > 0 ? '.inf' : '-.inf';
    return String(value);
  }
  return plainSafe(value) ? value : quoteString(value);
}

function keyText(key: string): string {
  return /^[A-Za-z_][A-Za-z0-9_.-]*$/.test(key) &&
    !/^(?:null|Null|NULL|true|True|TRUE|false|False|FALSE)$/.test(key)
    ? key
    : quoteString(key);
}

function isScalar(value: YamlValue): value is null | boolean | number | string {
  return value === null || typeof value !== 'object';
}

/** A short sequence of scalars, written as a flow sequence: [a, b]. */
function flowSequence(items: YamlValue[]): string | null {
  if (!items.every(isScalar)) return null;
  const text = `[${items.map((v) => scalar(v)).join(', ')}]`;
  return text.length <= 72 ? text : null;
}

function emit(value: YamlValue, indent: number, out: string[], prefix: string): void {
  const pad = ' '.repeat(indent);
  if (isScalar(value)) {
    out.push(`${prefix}${scalar(value)}`);
    return;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) {
      out.push(`${prefix}[]`);
      return;
    }
    if (prefix !== '') {
      const flow = flowSequence(value);
      if (flow !== null) {
        out.push(`${prefix}${flow}`);
        return;
      }
      out.push(prefix.trimEnd());
    }
    for (const item of value) {
      if (isScalar(item)) {
        out.push(`${pad}- ${scalar(item)}`);
      } else if (Array.isArray(item)) {
        emit(item, indent + 2, out, `${pad}- `);
      } else {
        const entries = Object.entries(item);
        if (entries.length === 0) {
          out.push(`${pad}- {}`);
          continue;
        }
        // The first key on the dash line, the others aligned under it.
        const lines: string[] = [];
        emit(item, indent + 2, lines, '');
        lines[0] = `${pad}- ${(lines[0] ?? '').slice(indent + 2)}`;
        out.push(...lines);
      }
    }
    return;
  }
  const entries = Object.entries(value);
  if (entries.length === 0) {
    out.push(`${prefix}{}`);
    return;
  }
  if (prefix !== '') out.push(prefix.trimEnd());
  for (const [key, item] of entries) {
    const head = `${pad}${keyText(key)}: `;
    if (isScalar(item)) {
      out.push(`${head}${scalar(item)}`);
    } else if (Array.isArray(item)) {
      const flow = item.length === 0 ? '[]' : flowSequence(item);
      if (flow !== null) {
        out.push(`${head}${flow}`);
      } else {
        out.push(head.trimEnd());
        emit(item, indent + 2, out, '');
      }
    } else if (Object.keys(item).length === 0) {
      out.push(`${head}{}`);
    } else {
      out.push(head.trimEnd());
      emit(item, indent + 2, out, '');
    }
  }
}

/** Removes the undefined members (as JSON.stringify would). */
function plain(value: unknown): YamlValue {
  return JSON.parse(JSON.stringify(value ?? null)) as YamlValue;
}

/** Writes a JSON-compatible value as a YAML document (block style). */
export function toYaml(value: unknown): string {
  const out: string[] = [];
  emit(plain(value), 0, out, '');
  return `${out.join('\n')}\n`;
}

/* ------------------------------------------------------------------------ */
/* Reader                                                                   */
/* ------------------------------------------------------------------------ */

class ParseError extends Error {
  constructor(
    readonly line: number,
    readonly column: number,
    message: string,
  ) {
    super(message);
  }
}

interface Line {
  /** 1-based line number. */
  no: number;
  indent: number;
  /** The content after the indentation, comments removed. */
  text: string;
  /** The raw line. */
  raw: string;
}

/** Index of a comment (" #" outside quotes) in the content of a line, or -1. */
function commentStart(text: string): number {
  let quote: string | null = null;
  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i];
    if (quote !== null) {
      if (quote === "'" && ch === "'" && text[i + 1] === "'") {
        i += 1;
      } else if (quote === '"' && ch === '\\') {
        i += 1;
      } else if (ch === quote) {
        quote = null;
      }
      continue;
    }
    if (ch === '#' && (i === 0 || text[i - 1] === ' ' || text[i - 1] === '\t')) return i;
    if ((ch === "'" || ch === '"') && (i === 0 || /[\s[{,:-]/.test(text[i - 1] ?? ''))) {
      quote = ch;
    }
  }
  return -1;
}

function splitLines(source: string): Line[] {
  const lines: Line[] = [];
  source.split(/\r?\n/).forEach((raw, i) => {
    if (raw.includes('\t') && /^\s*\t/.test(raw)) {
      throw new ParseError(i + 1, raw.indexOf('\t') + 1, 'tabs are not allowed in the indentation');
    }
    const indent = raw.length - raw.trimStart().length;
    let text = raw.slice(indent);
    const c = commentStart(text);
    if (c >= 0) text = text.slice(0, c);
    lines.push({ no: i + 1, indent, text: text.trimEnd(), raw });
  });
  return lines;
}

const intPattern = /^[-+]?(?:0|[1-9][0-9_]*)$/;
const floatPattern = /^[-+]?(?:\d[\d_]*)?(?:\.\d+)?(?:[eE][-+]?\d+)?$/;

function resolvePlain(text: string): YamlValue {
  if (text === '' || text === '~' || /^(?:null|Null|NULL)$/.test(text)) return null;
  if (/^(?:true|True|TRUE)$/.test(text)) return true;
  if (/^(?:false|False|FALSE)$/.test(text)) return false;
  if (intPattern.test(text)) return Number(text.replaceAll('_', ''));
  if (/^0x[0-9a-fA-F]+$/.test(text)) return Number.parseInt(text.slice(2), 16);
  if (/^0o[0-7]+$/.test(text)) return Number.parseInt(text.slice(2), 8);
  if (/\d/.test(text) && floatPattern.test(text)) return Number(text.replaceAll('_', ''));
  if (/^[-+]?\.(?:inf|Inf|INF)$/.test(text)) {
    return text.startsWith('-') ? -Infinity : Infinity;
  }
  if (/^\.(?:nan|NaN|NAN)$/.test(text)) return NaN;
  return text;
}

const escapes: Record<string, string> = {
  '0': '\0',
  a: '\x07',
  b: '\b',
  t: '\t',
  '\t': '\t',
  n: '\n',
  v: '\v',
  f: '\f',
  r: '\r',
  e: '\x1b',
  ' ': ' ',
  '"': '"',
  '/': '/',
  '\\': '\\',
  N: '\u0085',
  _: ' ',
};

/**
 * A cursor over the text of a flow collection or of a quoted scalar,
 * which may span lines.
 */
class Cursor {
  pos = 0;
  constructor(
    readonly text: string,
    /** Line and column of text[0]; newlines in text advance the line. */
    readonly line: number,
    readonly column: number,
  ) {}

  where(offset = this.pos): { line: number; column: number } {
    const before = this.text.slice(0, offset);
    const nl = before.lastIndexOf('\n');
    const lines = before.split('\n').length - 1;
    return {
      line: this.line + lines,
      column: nl < 0 ? this.column + offset : offset - nl,
    };
  }

  fail(message: string, offset = this.pos): never {
    const { line, column } = this.where(offset);
    throw new ParseError(line, column, message);
  }

  peek(): string {
    return this.text[this.pos] ?? '';
  }

  skipSpace(): void {
    while (this.pos < this.text.length) {
      const ch = this.text[this.pos];
      if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r') {
        this.pos += 1;
      } else {
        break;
      }
    }
  }

  atEnd(): boolean {
    return this.pos >= this.text.length;
  }
}

function readDoubleQuoted(c: Cursor): string {
  const start = c.pos;
  c.pos += 1;
  let out = '';
  while (!c.atEnd()) {
    const ch = c.peek();
    if (ch === '"') {
      c.pos += 1;
      return out;
    }
    if (ch === '\\') {
      const next = c.text[c.pos + 1] ?? '';
      if (next === 'x' || next === 'u' || next === 'U') {
        const len = next === 'x' ? 2 : next === 'u' ? 4 : 8;
        const hex = c.text.slice(c.pos + 2, c.pos + 2 + len);
        if (!new RegExp(`^[0-9a-fA-F]{${String(len)}}$`).test(hex)) {
          c.fail('invalid escape sequence');
        }
        out += String.fromCodePoint(Number.parseInt(hex, 16));
        c.pos += 2 + len;
        continue;
      }
      if (next === '\n') {
        // An escaped line break: the break and the leading spaces are dropped.
        c.pos += 2;
        while (c.peek() === ' ' || c.peek() === '\t') c.pos += 1;
        continue;
      }
      const mapped = escapes[next];
      if (mapped === undefined) c.fail(`invalid escape sequence \\${next}`);
      out += mapped;
      c.pos += 2;
      continue;
    }
    if (ch === '\n') {
      // A line break folds into a space (an empty line into a break).
      out = out.trimEnd();
      c.pos += 1;
      let breaks = 0;
      for (;;) {
        while (c.peek() === ' ' || c.peek() === '\t') c.pos += 1;
        if (c.peek() !== '\n') break;
        breaks += 1;
        c.pos += 1;
      }
      out += breaks > 0 ? '\n'.repeat(breaks) : ' ';
      continue;
    }
    out += ch;
    c.pos += 1;
  }
  return c.fail('unterminated double-quoted string', start);
}

function readSingleQuoted(c: Cursor): string {
  const start = c.pos;
  c.pos += 1;
  let out = '';
  while (!c.atEnd()) {
    const ch = c.peek();
    if (ch === "'") {
      if (c.text[c.pos + 1] === "'") {
        out += "'";
        c.pos += 2;
        continue;
      }
      c.pos += 1;
      return out;
    }
    if (ch === '\n') {
      out = out.trimEnd();
      c.pos += 1;
      while (c.peek() === ' ' || c.peek() === '\t') c.pos += 1;
      out += ' ';
      continue;
    }
    out += ch;
    c.pos += 1;
  }
  return c.fail('unterminated single-quoted string', start);
}

function checkIndicator(c: Cursor): void {
  const ch = c.peek();
  if (ch === '&' || ch === '*') c.fail('anchors and aliases are not supported');
  if (ch === '!') c.fail('tags are not supported');
  if (ch === '|' || ch === '>') c.fail('block scalars are not allowed in a flow collection');
}

/** A scalar inside a flow collection (stops at , ] } and ": "). */
function readFlowScalar(c: Cursor): YamlValue {
  const ch = c.peek();
  if (ch === '"') return readDoubleQuoted(c);
  if (ch === "'") return readSingleQuoted(c);
  checkIndicator(c);
  const start = c.pos;
  while (!c.atEnd()) {
    const cur = c.peek();
    if (cur === ',' || cur === ']' || cur === '}' || cur === '[' || cur === '{') break;
    if (cur === ':' && /[\s,\]}]/.test(c.text[c.pos + 1] ?? ' ')) break;
    c.pos += 1;
  }
  return resolvePlain(c.text.slice(start, c.pos).replace(/\s+/g, ' ').trim());
}

function readFlow(c: Cursor): YamlValue {
  c.skipSpace();
  const ch = c.peek();
  if (ch === '[') {
    c.pos += 1;
    const items: YamlValue[] = [];
    for (;;) {
      c.skipSpace();
      if (c.peek() === ']') {
        c.pos += 1;
        return items;
      }
      if (c.atEnd()) c.fail('unterminated flow sequence: missing ]');
      items.push(readFlow(c));
      c.skipSpace();
      if (c.atEnd()) c.fail('unterminated flow sequence: missing ]');
      if (c.peek() === ',') {
        c.pos += 1;
      } else if (c.peek() !== ']') {
        c.fail('expected , or ] in a flow sequence');
      }
    }
  }
  if (ch === '{') {
    c.pos += 1;
    const map: Record<string, YamlValue> = {};
    for (;;) {
      c.skipSpace();
      if (c.peek() === '}') {
        c.pos += 1;
        return map;
      }
      if (c.atEnd()) c.fail('unterminated flow mapping: missing }');
      const keyAt = c.pos;
      const key = readFlowScalar(c);
      c.skipSpace();
      let value: YamlValue = null;
      if (c.peek() === ':') {
        c.pos += 1;
        c.skipSpace();
        if (c.peek() !== ',' && c.peek() !== '}') value = readFlow(c);
      }
      if (typeof key === 'object' && key !== null) c.fail('complex keys are not supported', keyAt);
      const name = key === null ? 'null' : typeof key === 'string' ? key : String(key);
      if (Object.prototype.hasOwnProperty.call(map, name)) {
        c.fail(`duplicate key "${name}"`, keyAt);
      }
      map[name] = value;
      c.skipSpace();
      if (c.atEnd()) c.fail('unterminated flow mapping: missing }');
      if (c.peek() === ',') {
        c.pos += 1;
      } else if (c.peek() !== '}') {
        c.fail('expected , or } in a flow mapping');
      }
    }
  }
  return readFlowScalar(c);
}

/** True when the brackets of a flow collection are balanced in text. */
function flowClosed(text: string): boolean {
  let depth = 0;
  let quote: string | null = null;
  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i];
    if (quote !== null) {
      if (quote === '"' && ch === '\\') i += 1;
      else if (ch === quote) quote = null;
      continue;
    }
    if (ch === '"' || ch === "'") quote = ch;
    else if (ch === '[' || ch === '{') depth += 1;
    else if (ch === ']' || ch === '}') depth -= 1;
  }
  return depth <= 0 && quote === null;
}

class Parser {
  i = 0;
  constructor(readonly lines: Line[]) {}

  /** Skips the empty and comment-only lines. */
  skipBlank(): void {
    while (this.i < this.lines.length && (this.lines[this.i]?.text ?? '') === '') this.i += 1;
  }

  current(): Line | undefined {
    this.skipBlank();
    return this.lines[this.i];
  }

  /**
   * The value starting on the current line at column `indent` (a mapping,
   * a sequence or a scalar).
   */
  parseBlock(indent: number): YamlValue {
    const line = this.current();
    if (line === undefined || line.indent < indent) return null;
    if (line.text === '-' || line.text.startsWith('- ')) return this.parseSequence(line.indent);
    if (this.keyOf(line.text, line) !== null) return this.parseMapping(line.indent);
    return this.parseInline(line, line.text, line.indent, line.indent);
  }

  /**
   * The key of a "key: value" or "key:" line, with the offset of the value;
   * null when the line is not a mapping entry.
   */
  keyOf(text: string, line: Line): { key: string; rest: number } | null {
    if (text.startsWith('"') || text.startsWith("'")) {
      const c = new Cursor(text, line.no, line.indent + 1);
      const key = text.startsWith('"') ? readDoubleQuoted(c) : readSingleQuoted(c);
      const after = text.slice(c.pos);
      if (after === ':' || after.startsWith(': ')) return { key, rest: c.pos + 1 };
      return null;
    }
    if (text.startsWith('[') || text.startsWith('{') || text === '-' || text.startsWith('- ')) {
      return null;
    }
    const m = /^(.*?[^\s]):(?:\s|$)/.exec(text);
    if (m?.[1] === undefined) return null;
    const key = m[1].trim();
    if (key === '' || key.includes(': ')) return null;
    if (key.startsWith('?')) {
      throw new ParseError(line.no, line.indent + 1, 'complex keys are not supported');
    }
    return { key, rest: m[0].length - (m[0].endsWith(':') ? 0 : 1) };
  }

  parseMapping(indent: number): Record<string, YamlValue> {
    const map: Record<string, YamlValue> = {};
    for (;;) {
      const line = this.current();
      if (line === undefined || line.indent < indent) return map;
      if (line.indent > indent) {
        throw new ParseError(line.no, line.indent + 1, 'unexpected indentation');
      }
      if (line.text === '-' || line.text.startsWith('- ')) {
        throw new ParseError(line.no, line.indent + 1, 'a sequence item where a key is expected');
      }
      const entry = this.keyOf(line.text, line);
      if (entry === null) {
        throw new ParseError(line.no, line.indent + 1, 'expected "key: value"');
      }
      if (Object.prototype.hasOwnProperty.call(map, entry.key)) {
        throw new ParseError(line.no, line.indent + 1, `duplicate key "${entry.key}"`);
      }
      map[entry.key] = this.parseValueAfter(line, entry.rest, indent);
    }
  }

  parseSequence(indent: number): YamlValue[] {
    const items: YamlValue[] = [];
    for (;;) {
      const line = this.current();
      if (line === undefined || line.indent < indent) return items;
      if (line.indent > indent) {
        throw new ParseError(line.no, line.indent + 1, 'unexpected indentation');
      }
      if (!(line.text === '-' || line.text.startsWith('- '))) return items;
      const rest = line.text === '-' ? '' : line.text.slice(2);
      const restTrimmed = rest.trimStart();
      const column = line.indent + 2 + (rest.length - restTrimmed.length);
      if (restTrimmed === '') {
        this.i += 1;
        const next = this.current();
        items.push(
          next !== undefined && next.indent > indent ? this.parseBlock(next.indent) : null,
        );
        continue;
      }
      // "- key: value" opens a mapping whose keys align after the dash.
      const virtual: Line = { ...line, indent: column, text: restTrimmed };
      if (
        restTrimmed === '-' ||
        restTrimmed.startsWith('- ') ||
        this.keyOf(restTrimmed, virtual) !== null
      ) {
        this.lines[this.i] = virtual;
        items.push(this.parseBlock(column));
        continue;
      }
      items.push(this.parseInline(line, restTrimmed, column, indent));
    }
  }

  /** The value of a mapping entry: on the same line or in the block below. */
  parseValueAfter(line: Line, rest: number, indent: number): YamlValue {
    const text = line.text.slice(rest);
    const trimmed = text.trimStart();
    if (trimmed === '') {
      this.i += 1;
      const next = this.current();
      if (next === undefined) return null;
      // A sequence may sit at the indentation of its key.
      if (next.indent === indent && (next.text === '-' || next.text.startsWith('- '))) {
        return this.parseSequence(indent);
      }
      if (next.indent > indent) return this.parseBlock(next.indent);
      return null;
    }
    const column = line.indent + rest + (text.length - trimmed.length);
    return this.parseInline(line, trimmed, column, indent);
  }

  /**
   * A value written on the line (scalar, flow collection, block scalar
   * header), possibly continued on the next lines. Consumes the lines.
   */
  parseInline(line: Line, text: string, column: number, parentIndent: number): YamlValue {
    const first = text[0];
    if (first === '&' || first === '*') {
      throw new ParseError(line.no, column + 1, 'anchors and aliases are not supported');
    }
    if (first === '!') throw new ParseError(line.no, column + 1, 'tags are not supported');
    if (first === '|' || first === '>')
      return this.parseBlockScalar(line, text, column, parentIndent);
    if (first === '[' || first === '{' || first === '"' || first === "'") {
      // Gather the continuation lines of a multi-line flow or quoted value.
      let source = text;
      let end = this.i;
      const flow = first === '[' || first === '{';
      const closed = (s: string) => (flow ? flowClosed(s) : quotedClosed(s));
      while (!closed(source) && end + 1 < this.lines.length) {
        end += 1;
        const next = this.lines[end];
        // Comments end the lines of a flow collection, not those of a string.
        source += `\n${flow ? (next?.text ?? '') : (next?.raw ?? '').trim()}`;
      }
      const c = new Cursor(source, line.no, column + 1);
      const value =
        first === '"' ? readDoubleQuoted(c) : first === "'" ? readSingleQuoted(c) : readFlow(c);
      const trailing = source.slice(c.pos).trim();
      if (trailing !== '' && !trailing.startsWith('#')) {
        c.fail('unexpected text after the value', c.pos);
      }
      this.i = end + 1;
      return value;
    }
    // A plain scalar, continued by the more indented lines.
    let value = text;
    this.i += 1;
    for (;;) {
      const next = this.lines[this.i];
      if (next === undefined || next.text === '' || next.indent <= parentIndent) break;
      if (this.keyOf(next.text, next) !== null || next.text.startsWith('- ')) break;
      value += ` ${next.text}`;
      this.i += 1;
    }
    if (/:\s/.test(value)) {
      throw new ParseError(line.no, column + 1, 'a mapping value is not allowed here');
    }
    return resolvePlain(value);
  }

  /** A literal (|) or folded (>) block scalar. */
  parseBlockScalar(line: Line, header: string, column: number, parentIndent: number): string {
    const m = /^([|>])([-+]?)\s*$/.exec(header);
    if (m === null) {
      throw new ParseError(line.no, column + 1, 'unsupported block scalar header');
    }
    const folded = m[1] === '>';
    const chomp = m[2] ?? '';
    this.i += 1;
    const body: string[] = [];
    let blockIndent = -1;
    while (this.i < this.lines.length) {
      const raw = this.lines[this.i]?.raw ?? '';
      const indent = raw.length - raw.trimStart().length;
      if (raw.trim() === '') {
        body.push('');
        this.i += 1;
        continue;
      }
      if (blockIndent < 0) {
        if (indent <= parentIndent) break;
        blockIndent = indent;
      }
      if (indent < blockIndent) break;
      body.push(raw.slice(blockIndent));
      this.i += 1;
    }
    // Trailing empty lines belong to the chomping, not to the next node.
    let trailingEmpty = 0;
    while (body.length > 0 && body[body.length - 1] === '') {
      body.pop();
      trailingEmpty += 1;
    }
    let text: string;
    if (folded) {
      text = '';
      body.forEach((l, idx) => {
        if (idx === 0) text = l;
        else if (l === '' || l.startsWith(' ')) text += `\n${l}`;
        else text += body[idx - 1] === '' ? l : ` ${l}`;
      });
    } else {
      text = body.join('\n');
    }
    if (chomp === '-') return text;
    if (chomp === '+') return `${text}\n${'\n'.repeat(trailingEmpty)}`;
    return body.length === 0 ? '' : `${text}\n`;
  }
}

function quotedClosed(text: string): boolean {
  const q = text[0];
  for (let i = 1; i < text.length; i += 1) {
    const ch = text[i];
    if (q === '"' && ch === '\\') {
      i += 1;
      continue;
    }
    if (ch === q) {
      if (q === "'" && text[i + 1] === "'") {
        i += 1;
        continue;
      }
      return true;
    }
  }
  return false;
}

/** Parses a YAML document (a single one; "---" may start it). */
export function parseYaml(source: string): YamlParseResult {
  try {
    const lines = splitLines(source);
    let start = 0;
    while (start < lines.length && (lines[start]?.text ?? '') === '') start += 1;
    if (lines[start]?.text === '---') {
      lines[start] = { ...(lines[start] as Line), text: '' };
    }
    const docEnd = lines.findIndex(
      (l, idx) => idx > start && (l.text === '---' || l.text === '...'),
    );
    if (docEnd >= 0) {
      const extra = lines.slice(docEnd + 1).some((l) => l.text !== '');
      if (lines[docEnd]?.text === '---' || extra) {
        const at = lines[docEnd] as Line;
        throw new ParseError(at.no, 1, 'only one YAML document is supported');
      }
      lines.length = docEnd;
    }
    const parser = new Parser(lines);
    const first = parser.current();
    if (first === undefined) return { ok: true, value: null };
    const value = parser.parseBlock(first.indent);
    const rest = parser.current();
    if (rest !== undefined) {
      throw new ParseError(rest.no, rest.indent + 1, 'unexpected content');
    }
    return { ok: true, value };
  } catch (error) {
    if (error instanceof ParseError) {
      return {
        ok: false,
        error: { line: error.line, column: error.column, message: error.message },
      };
    }
    throw error;
  }
}
