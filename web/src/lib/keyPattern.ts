/**
 * Key patterns of the access rules and of the behaviors of the
 * distributions (docs/10 §10.1), evaluated in the browser for the live
 * previews of the editors and by the mocks: the same translation of the
 * globs as the server (internal/service/match), RE2 expressions checked for
 * the constructs RE2 refuses and run with the JavaScript engine, keys
 * normalized like the server (Unicode NFC, "//", "." and ".." resolved).
 *
 * The server stays the reference: an expression the browser cannot run is
 * reported as such (`unsupported`) and validated when the rules are saved.
 */
import type { KeyPattern } from '@/services';

/** Maximum length of a pattern (characters), like the server. */
export const maxPatternLength = 1024;

/** A pattern that does not compile: 1-based position in the pattern (0: unknown). */
export interface PatternIssue {
  position: number;
  message: string;
  /**
   * The browser cannot evaluate the expression although RE2 may accept it
   * (a syntax JavaScript lacks): the server decides when the rules are saved.
   */
  unsupported?: boolean;
}

export interface PatternMatch {
  matched: boolean;
  /** Named groups captured by a regex. */
  captures: Record<string, string>;
}

/** A compiled pattern. */
export interface CompiledPattern {
  readonly pattern: KeyPattern;
  /** Names of the named groups of a regex. */
  readonly groups: readonly string[];
  match(key: string): PatternMatch;
}

export type CompileResult =
  { ok: true; compiled: CompiledPattern } | { ok: false; issue: PatternIssue };

const metaChars = new Set(['\\', '^', '$', '.', '|', '?', '*', '+', '(', ')', '[', ']', '{', '}']);

function quoteMeta(ch: string): string {
  return metaChars.has(ch) ? `\\${ch}` : ch;
}

function classEscape(ch: string): string {
  return ['\\', ']', '[', '^', '-'].includes(ch) ? `\\${ch}` : ch;
}

/**
 * Translates a glob into the body of an anchored regular expression, as
 * internal/service/match does: "*" stays in a segment, "**" crosses them
 * ("**\/" at the start of a segment also matches no segment), "?" is one
 * character of a segment, "[...]" a class ("!" or "^" negates it, never
 * matching "/"), "{a,b}" alternatives, "\" escapes.
 */
export function globToRegexSource(glob: string): { source: string } | { issue: PatternIssue } {
  const src = [...glob];
  let pos = 0;
  let out = '';

  const fail = (position: number, message: string) => ({ issue: { position, message } });

  function classChar(): { ch: string } | { issue: PatternIssue } {
    const ch = src[pos] ?? '';
    if (ch === '\\') {
      if (pos + 1 >= src.length) return fail(pos + 1, 'trailing backslash');
      pos += 2;
      return { ch: src[pos - 1] ?? '' };
    }
    pos += 1;
    return { ch };
  }

  function charClass(): PatternIssue | null {
    const start = pos;
    pos += 1; // '['
    let negated = false;
    if (pos < src.length && (src[pos] === '!' || src[pos] === '^')) {
      negated = true;
      pos += 1;
    }
    let body = '';
    let first = true;
    for (;;) {
      if (pos >= src.length)
        return { position: start + 1, message: 'unterminated character class' };
      if (src[pos] === ']' && !first) {
        pos += 1;
        break;
      }
      first = false;
      const lo = classChar();
      if ('issue' in lo) return lo.issue;
      if (pos + 1 < src.length && src[pos] === '-' && src[pos + 1] !== ']') {
        const rangePos = pos;
        pos += 1;
        const hi = classChar();
        if ('issue' in hi) return hi.issue;
        if ((hi.ch.codePointAt(0) ?? 0) < (lo.ch.codePointAt(0) ?? 0)) {
          return {
            position: rangePos + 1,
            message: `invalid character class range ${lo.ch}-${hi.ch}`,
          };
        }
        body += `${classEscape(lo.ch)}-${classEscape(hi.ch)}`;
        continue;
      }
      body += classEscape(lo.ch);
    }
    out += `[${negated ? '^/' : ''}${body}]`;
    return null;
  }

  function star(segStart: boolean) {
    let n = 0;
    while (pos < src.length && src[pos] === '*') {
      n += 1;
      pos += 1;
    }
    if (n === 1) {
      out += '[^/]*';
      return;
    }
    if (segStart && pos < src.length && src[pos] === '/') {
      out += '(?:.*/)?';
      pos += 1;
      return;
    }
    out += '.*';
  }

  function braces(segStart: boolean): PatternIssue | null {
    const start = pos;
    pos += 1; // '{'
    out += '(?:';
    for (;;) {
      const issue = sequence(segStart, true);
      if (issue !== null) return issue;
      if (pos >= src.length) return { position: start + 1, message: 'unterminated alternation' };
      if (src[pos] === '}') {
        pos += 1;
        out += ')';
        return null;
      }
      pos += 1; // ','
      out += '|';
    }
  }

  function sequence(segStartIn: boolean, inBraces: boolean): PatternIssue | null {
    let segStart = segStartIn;
    while (pos < src.length) {
      const ch = src[pos] ?? '';
      if (inBraces && (ch === ',' || ch === '}')) return null;
      if (ch === '\\') {
        if (pos + 1 >= src.length) return { position: pos + 1, message: 'trailing backslash' };
        out += quoteMeta(src[pos + 1] ?? '');
        pos += 2;
        segStart = false;
      } else if (ch === '*') {
        star(segStart);
        segStart = false;
      } else if (ch === '?') {
        out += '[^/]';
        pos += 1;
        segStart = false;
      } else if (ch === '[') {
        const issue = charClass();
        if (issue !== null) return issue;
        segStart = false;
      } else if (ch === '{') {
        const issue = braces(segStart);
        if (issue !== null) return issue;
        segStart = false;
      } else {
        out += quoteMeta(ch);
        pos += 1;
        segStart = ch === '/';
      }
    }
    return null;
  }

  const issue = sequence(true, false);
  if (issue !== null) return { issue };
  return { source: out };
}

/**
 * Checks an RE2 expression and translates it into a JavaScript one:
 * backreferences and lookarounds are refused (RE2 runs in linear time and
 * has neither), "(?P<name>" becomes "(?<name>", the leading flags "(?i)",
 * "(?s)", "(?m)" become flags of the expression.
 */
export function re2ToJs(
  expr: string,
): { source: string; flags: string; groups: string[] } | { issue: PatternIssue } {
  let flags = '';
  let body = expr;
  let offset = 0;
  const leading = /^\(\?([imsU]+)\)/.exec(body);
  if (leading?.[1] !== undefined) {
    if (leading[1].includes('U')) {
      return {
        issue: {
          position: 3,
          message: 'the ungreedy flag U is not supported by the preview',
          unsupported: true,
        },
      };
    }
    flags = [...new Set(leading[1])].join('');
    body = body.slice(leading[0].length);
    offset = leading[0].length;
  }
  const chars = [...body];
  let out = '';
  const groups: string[] = [];
  let inClass = false;
  for (let i = 0; i < chars.length; i += 1) {
    const ch = chars[i] ?? '';
    const position = offset + i + 1;
    if (ch === '\\') {
      const next = chars[i + 1];
      if (next === undefined) return { issue: { position, message: 'trailing backslash' } };
      if (!inClass && /[1-9]/.test(next)) {
        return {
          issue: { position, message: 'backreferences are not supported (RE2)' },
        };
      }
      if (next === 'z' && !inClass) {
        out += '$';
      } else if (next === 'A' && !inClass) {
        out += '^';
      } else if (next === 'C' || next === 'Q' || next === 'E') {
        return {
          issue: {
            position,
            message: `\\${next} is not supported by the preview`,
            unsupported: true,
          },
        };
      } else {
        out += `\\${next}`;
      }
      i += 1;
      continue;
    }
    if (inClass) {
      if (ch === '[' && chars[i + 1] === ':') {
        return {
          issue: {
            position,
            message: 'POSIX classes such as [:alpha:] are not supported by the preview',
            unsupported: true,
          },
        };
      }
      if (ch === ']') inClass = false;
      out += ch;
      continue;
    }
    if (ch === '[') {
      inClass = true;
      out += ch;
      if (chars[i + 1] === '^') {
        out += '^';
        i += 1;
      }
      if (chars[i + 1] === ']') {
        out += '\\]';
        i += 1;
      }
      continue;
    }
    if (ch === '(' && chars[i + 1] === '?') {
      const rest = chars.slice(i + 2, i + 5).join('');
      if (
        rest.startsWith('=') ||
        rest.startsWith('!') ||
        rest.startsWith('<=') ||
        rest.startsWith('<!')
      ) {
        return {
          issue: { position, message: 'lookaround assertions are not supported (RE2)' },
        };
      }
      const named = /^\(\?P?<([A-Za-z_][A-Za-z0-9_]*)>/.exec(chars.slice(i).join(''));
      if (named?.[1] !== undefined) {
        groups.push(named[1]);
        out += `(?<${named[1]}>`;
        i += named[0].length - 1;
        continue;
      }
      if (chars[i + 2] === ':') {
        out += '(?:';
        i += 2;
        continue;
      }
      return {
        issue: {
          position,
          message: 'this group syntax is not supported by the preview',
          unsupported: true,
        },
      };
    }
    out += ch;
  }
  if (inClass) return { issue: { position: expr.length, message: 'missing closing ]' } };
  return { source: out, flags, groups };
}

/** The message of a JavaScript RegExp syntax error, without its prefix. */
function syntaxMessage(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  return message.replace(/^Invalid regular expression: /, '');
}

/**
 * Compiles a pattern of a rule or a behavior. Prefixes compare the start
 * of the key (an empty prefix matches every key), globs match the whole
 * key, regexes search the key (anchor them with ^ and $).
 */
export function compileKeyPattern(pattern: KeyPattern): CompileResult {
  const text = pattern.pattern;
  if ([...text].length > maxPatternLength) {
    return {
      ok: false,
      issue: {
        position: maxPatternLength + 1,
        message: `the pattern is longer than ${String(maxPatternLength)} characters`,
      },
    };
  }
  const insensitive = pattern.caseInsensitive === true;
  if (pattern.type === 'prefix') {
    const prefix = insensitive ? text.toLowerCase() : text;
    return {
      ok: true,
      compiled: {
        pattern,
        groups: [],
        match(key) {
          const k = insensitive ? key.toLowerCase() : key;
          return { matched: k.startsWith(prefix), captures: {} };
        },
      },
    };
  }
  let source: string;
  let flags = 'u';
  let groups: string[] = [];
  if (pattern.type === 'glob') {
    const translated = globToRegexSource(text);
    if ('issue' in translated) return { ok: false, issue: translated.issue };
    source = `^${translated.source}$`;
    flags += 's';
  } else {
    if (text === '') {
      return { ok: false, issue: { position: 1, message: 'the expression is empty' } };
    }
    const translated = re2ToJs(text);
    if ('issue' in translated) return { ok: false, issue: translated.issue };
    source = translated.source;
    flags += translated.flags;
    groups = translated.groups;
  }
  if (insensitive && !flags.includes('i')) flags += 'i';
  let re: RegExp;
  try {
    re = new RegExp(source, flags);
  } catch (error) {
    // The Unicode mode refuses escapes RE2 accepts ("\-", "\ "): retry
    // without it before reporting a syntax error.
    try {
      re = new RegExp(source, flags.replace('u', ''));
    } catch {
      return { ok: false, issue: { position: 0, message: syntaxMessage(error) } };
    }
  }
  return {
    ok: true,
    compiled: {
      pattern,
      groups,
      match(key) {
        const m = re.exec(key);
        if (m === null) return { matched: false, captures: {} };
        const captures: Record<string, string> = {};
        for (const [name, value] of Object.entries(m.groups ?? {})) {
          if (value !== undefined) captures[name] = value;
        }
        return { matched: true, captures };
      },
    },
  };
}

/**
 * The canonical form of a key, as the server matches it: Unicode NFC,
 * then the empty, "." and ".." segments resolved lexically ("a//b",
 * "./a/b" and "x/../a/b" become "a/b"; a ".." above the root is dropped;
 * a leading and a trailing "/" are kept).
 */
export function normalizeKey(key: string): string {
  const nfc = key.normalize('NFC');
  if (!needsClean(nfc)) return nfc;
  const leading = nfc.startsWith('/');
  const trailing =
    nfc.endsWith('/') || nfc.endsWith('/.') || nfc.endsWith('/..') || nfc === '.' || nfc === '..';
  const out: string[] = [];
  for (const seg of nfc.split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') {
      out.pop();
      continue;
    }
    out.push(seg);
  }
  return `${leading ? '/' : ''}${out.join('/')}${trailing && out.length > 0 ? '/' : ''}`;
}

function needsClean(key: string): boolean {
  if (key === '.' || key === '..' || key.includes('//')) return true;
  if (key.includes('/./') || key.includes('/../')) return true;
  return key.startsWith('./') || key.startsWith('../') || key.endsWith('/.') || key.endsWith('/..');
}

/** A short form of a pattern for tables: "glob docs/**\/*.pdf (i)". */
export function describePattern(pattern: KeyPattern | undefined): string {
  if (pattern === undefined) return '';
  return `${pattern.pattern}${pattern.caseInsensitive === true ? ' (i)' : ''}`;
}

/**
 * The problem of a pattern found in the browser, or none: an empty glob or
 * regex is not reported (the editor asks for it), an expression the
 * browser cannot run is reported as unsupported (the server decides).
 */
export function patternIssue(value: KeyPattern | undefined): PatternIssue | undefined {
  if (value === undefined) return undefined;
  if (value.type !== 'prefix' && value.pattern === '') return undefined;
  const result = compileKeyPattern(value);
  return result.ok ? undefined : result.issue;
}
