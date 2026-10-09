import { describe, expect, it } from 'vitest';

// The raw files: i18next merges the sections loaded on demand into the
// bundled resources, the imported JSON modules of the default language.
import enText from './locales/en.json?raw';
import enSectionsText from './locales/en.sections.json?raw';
import itText from './locales/it.json?raw';
import itSectionsText from './locales/it.sections.json?raw';
import { detectLanguage } from './index';

type Tree = { [key: string]: string | Tree };

/** Flattens a nested JSON object into dotted keys -> leaf strings. */
function flatten(tree: Tree, prefix = ''): Map<string, string> {
  const out = new Map<string, string>();
  for (const [key, value] of Object.entries(tree)) {
    const path = prefix === '' ? key : `${prefix}.${key}`;
    if (typeof value === 'string') {
      out.set(path, value);
    } else {
      for (const [k, v] of flatten(value, path)) {
        out.set(k, v);
      }
    }
  }
  return out;
}

/** Extracts the interpolation placeholders ({{name}}) of a message, sorted. */
function placeholders(message: string): string[] {
  return [...message.matchAll(/{{\s*([\w.]+)\s*}}/g)].map((m) => m[1] ?? '').sort();
}

const en = JSON.parse(enText) as Tree;
const enSections = JSON.parse(enSectionsText) as Tree;
const it_ = JSON.parse(itText) as Tree;
const itSections = JSON.parse(itSectionsText) as Tree;

describe('i18n resources', () => {
  const enFlat = flatten({ ...en, ...enSections });
  const itFlat = flatten({ ...it_, ...itSections });

  it('keep the core and the sections apart', () => {
    for (const [core, sections] of [
      [en, enSections],
      [it_, itSections],
    ] as const) {
      for (const key of Object.keys(sections)) {
        expect(Object.keys(core), key).not.toContain(key);
      }
    }
  });

  it('have the same keys in it and en', () => {
    expect([...itFlat.keys()].sort()).toEqual([...enFlat.keys()].sort());
  });

  it('have no empty messages', () => {
    for (const [lang, flat] of [
      ['en', enFlat],
      ['it', itFlat],
    ] as const) {
      for (const [key, value] of flat) {
        expect(value.trim(), `${lang}:${key}`).not.toBe('');
      }
    }
  });

  it('use the same interpolation placeholders in both languages', () => {
    for (const [key, enValue] of enFlat) {
      const itValue = itFlat.get(key) ?? '';
      expect(placeholders(itValue), key).toEqual(placeholders(enValue));
    }
  });
});

describe('detectLanguage', () => {
  it('prefers the stored language', () => {
    expect(detectLanguage('it', ['en-US'])).toBe('it');
  });

  it('falls back to the first supported browser language', () => {
    expect(detectLanguage(null, ['fr-FR', 'it-IT', 'en'])).toBe('it');
    expect(detectLanguage('xx', ['en-GB'])).toBe('en');
  });

  it('defaults to english', () => {
    expect(detectLanguage(undefined, ['fr', 'de'])).toBe('en');
    expect(detectLanguage(null, [])).toBe('en');
  });
});
