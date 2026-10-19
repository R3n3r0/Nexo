import { baseName } from '@/lib/objectPath';
import type { ObjectEntry, ObjectList } from '@/services';

/** A row of the object browser: a folder (common prefix) or an object. */
export type BrowserRow =
  | { kind: 'folder'; id: string; key: string; name: string }
  | { kind: 'object'; id: string; key: string; name: string; entry: ObjectEntry };

/**
 * Rows of the loaded pages of a listing: folders first, then objects, in
 * listing order (each page is sorted by the server), optionally filtered
 * by a case-insensitive substring of the name. The folders of hidden are
 * left out (the outputs of the media worker).
 */
export function buildRows(
  pages: readonly ObjectList[],
  filter = '',
  hidden: readonly string[] = [],
): BrowserRow[] {
  const needle = filter.trim().toLowerCase();
  const matches = (name: string) => needle === '' || name.toLowerCase().includes(needle);
  const folders: BrowserRow[] = [];
  const objects: BrowserRow[] = [];
  const seen = new Set<string>();
  for (const page of pages) {
    for (const prefix of page.prefixes) {
      const name = baseName(prefix);
      if (!seen.has(prefix) && !hidden.includes(prefix) && matches(name)) {
        seen.add(prefix);
        folders.push({ kind: 'folder', id: prefix, key: prefix, name });
      }
    }
    for (const entry of page.objects) {
      const name = baseName(entry.key);
      if (!seen.has(entry.key) && matches(name)) {
        seen.add(entry.key);
        objects.push({ kind: 'object', id: entry.key, key: entry.key, name, entry });
      }
    }
  }
  return [...folders, ...objects];
}
