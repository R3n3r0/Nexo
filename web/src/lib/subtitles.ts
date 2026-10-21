import type { SubtitleCue } from '@/services/types';

/** The bounds of a subtitle track (domain.MaxSubtitleBytes, MaxSubtitleCueText). */
export const maxSubtitleBytes = 2 << 20;
export const maxCueText = 2048;

/** The canonical BCP 47 tag of a language ("pt-br" → "pt-BR"), or undefined. */
export function canonicalLanguage(tag: string): string | undefined {
  const t = tag.trim();
  if (!/^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$/.test(t) || t.length > 35) return undefined;
  try {
    const [out] = Intl.getCanonicalLocales(t);
    return out === undefined || out === 'und' ? undefined : out;
  } catch {
    return undefined;
  }
}

/** The name of a language in that language ("Italiano"), else the tag. */
export function languageName(tag: string): string {
  try {
    const name = new Intl.DisplayNames([tag], { type: 'language' }).of(tag);
    if (name === undefined || name === tag) return tag;
    return name.charAt(0).toLocaleUpperCase(tag) + name.slice(1);
  } catch {
    return tag;
  }
}

/** Seconds as a WebVTT timestamp (hh:mm:ss.mmm), or with a comma for SubRip. */
export function formatCueTime(seconds: number, separator: '.' | ',' = '.'): string {
  const ms = Math.max(0, Math.round(seconds * 1000));
  const h = Math.floor(ms / 3_600_000);
  const m = Math.floor((ms % 3_600_000) / 60_000);
  const s = Math.floor((ms % 60_000) / 1000);
  const pad = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${pad(h)}:${pad(m)}:${pad(s)}${separator}${pad(ms % 1000, 3)}`;
}

/** A timestamp (hh:mm:ss.mmm, mm:ss.mmm, with "." or ",") in seconds, or undefined. */
export function parseCueTime(text: string): number | undefined {
  const m = /^(?:(\d+):)?([0-5]\d):([0-5]\d)[.,](\d{3})$/.exec(text.trim());
  if (m === null) return undefined;
  const [, h = '0', min = '0', s = '0', ms = '0'] = m;
  return Number(h) * 3600 + Number(min) * 60 + Number(s) + Number(ms) / 1000;
}

/** Why a cue is invalid, or undefined. */
export function cueProblem(cue: SubtitleCue): 'text' | 'order' | 'long' | undefined {
  if (cue.text.trim() === '') return 'text';
  if (!(cue.end > cue.start) || cue.start < 0) return 'order';
  if (new TextEncoder().encode(cue.text).length > maxCueText) return 'long';
  return undefined;
}

/**
 * Parses a WebVTT or SubRip document: the cues sorted by start time.
 * Throws on a malformed timing line.
 */
export function parseSubtitles(doc: string): SubtitleCue[] {
  const blocks = doc
    .replace(/^\uFEFF/, '')
    .replace(/\r\n?/g, '\n')
    .split(/\n{2,}/);
  const cues: SubtitleCue[] = [];
  for (const block of blocks) {
    const lines = block.split('\n').filter((l, i) => i > 0 || l.trim() !== '');
    const at = lines.findIndex((l) => l.includes('-->'));
    if (at < 0 || at > 1) continue; // header, NOTE, STYLE, REGION
    const timing = lines[at] ?? '';
    const [startText = '', rest = ''] = timing.split('-->');
    const [endText = '', ...settings] = rest.trim().split(/\s+/);
    const start = parseCueTime(startText);
    const end = parseCueTime(endText);
    if (start === undefined || end === undefined) {
      throw new Error(`malformed timing: ${timing.trim()}`);
    }
    const text = lines
      .slice(at + 1)
      .map((l) => l.trim())
      .filter((l) => l !== '')
      .join('\n');
    if (text === '') continue;
    const cue: SubtitleCue = { start, end, text };
    if (settings.length > 0) cue.settings = settings.join(' ');
    cues.push(cue);
  }
  return cues.sort((a, b) => a.start - b.start || a.end - b.end);
}

/** The cues as a WebVTT document. */
export function formatWebVTT(cues: readonly SubtitleCue[]): string {
  const blocks = cues.map((c) => {
    const settings = c.settings !== undefined && c.settings !== '' ? ` ${c.settings}` : '';
    return `${formatCueTime(c.start)} --> ${formatCueTime(c.end)}${settings}\n${c.text}`;
  });
  return ['WEBVTT', ...blocks].join('\n\n') + '\n';
}

/** The cues as a SubRip document. */
export function formatSRT(cues: readonly SubtitleCue[]): string {
  return cues
    .map(
      (c, i) =>
        `${String(i + 1)}\n${formatCueTime(c.start, ',')} --> ${formatCueTime(c.end, ',')}\n${c.text}\n`,
    )
    .join('\n');
}
