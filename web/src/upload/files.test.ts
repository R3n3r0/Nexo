import { describe, expect, it } from 'vitest';

import { filesFromDataTransfer, filesFromInput, keyFor } from './files';

/** Minimal File System entries, as webkitGetAsEntry returns them. */
function fileEntry(name: string, content = name): FileSystemFileEntry {
  return {
    name,
    isFile: true,
    isDirectory: false,
    file: (success: (file: File) => void) => {
      success(new File([content], name));
    },
  } as unknown as FileSystemFileEntry;
}

function dirEntry(name: string, children: FileSystemEntry[]): FileSystemDirectoryEntry {
  return {
    name,
    isFile: false,
    isDirectory: true,
    createReader: () => {
      // Two batches, then an empty one, like Chrome (100 entries per call).
      const batches = [children.slice(0, 1), children.slice(1), []];
      return {
        readEntries: (success: (entries: FileSystemEntry[]) => void) => {
          success(batches.shift() ?? []);
        },
      };
    },
  } as unknown as FileSystemDirectoryEntry;
}

function transfer(entries: FileSystemEntry[]): DataTransfer {
  return {
    items: entries.map((entry) => ({ kind: 'file', webkitGetAsEntry: () => entry })),
    files: [],
  } as unknown as DataTransfer;
}

describe('dropped folders (D-OBJ-02)', () => {
  it('walks folders recursively and keeps their structure', async () => {
    const dropped = transfer([
      dirEntry('album', [
        fileEntry('a.jpg'),
        dirEntry('2026', [fileEntry('b.jpg'), fileEntry('c d#.jpg')]),
      ]),
      fileEntry('loose.txt'),
    ]);
    const files = await filesFromDataTransfer(dropped);
    expect(files.map((f) => f.path)).toEqual([
      'album/a.jpg',
      'album/2026/b.jpg',
      'album/2026/c d#.jpg',
      'loose.txt',
    ]);
    expect(files.map((f) => keyFor('photos/', f.path))).toEqual([
      'photos/album/a.jpg',
      'photos/album/2026/b.jpg',
      'photos/album/2026/c d#.jpg',
      'photos/loose.txt',
    ]);
  });

  it('falls back to the flat file list without entries', async () => {
    const file = new File(['x'], 'x.txt');
    const files = await filesFromDataTransfer({
      items: [],
      files: [file],
    } as unknown as DataTransfer);
    expect(files).toEqual([{ file, path: 'x.txt' }]);
  });

  it('uses webkitRelativePath of folder pickers', () => {
    const file = new File(['x'], 'x.txt');
    Object.defineProperty(file, 'webkitRelativePath', { value: 'dir/sub/x.txt' });
    expect(filesFromInput([file])).toEqual([{ file, path: 'dir/sub/x.txt' }]);
    expect(filesFromInput(null)).toEqual([]);
  });

  it('drops unsafe path segments from keys', () => {
    expect(keyFor('', '../a/./b//c.txt')).toBe('a/b/c.txt');
    expect(keyFor('p/', 'win\\path\\f.txt')).toBe('p/win/path/f.txt');
  });
});
