/** A file picked or dropped, with its path relative to the drop target. */
export interface PickedFile {
  file: File;
  /** "a/b/c.txt" for a file of a dropped folder, the file name otherwise. */
  path: string;
}

/**
 * Files of an <input type="file"> (multiple or webkitdirectory): folder
 * pickers fill webkitRelativePath with "folder/sub/file".
 */
export function filesFromInput(list: FileList | readonly File[] | null): PickedFile[] {
  if (list === null) {
    return [];
  }
  return Array.from(list, (file) => {
    // Not every runtime implements webkitRelativePath (undefined, not "").
    const relative = (file.webkitRelativePath as string | undefined) ?? '';
    return { file, path: relative !== '' ? relative : file.name };
  });
}

/** Reads every entry of a directory (readEntries returns them in batches). */
function readAllEntries(reader: FileSystemDirectoryReader): Promise<FileSystemEntry[]> {
  return new Promise((resolve, reject) => {
    const entries: FileSystemEntry[] = [];
    const next = () => {
      reader.readEntries((batch) => {
        if (batch.length === 0) {
          resolve(entries);
          return;
        }
        entries.push(...batch);
        next();
      }, reject);
    };
    next();
  });
}

function fileOf(entry: FileSystemFileEntry): Promise<File> {
  return new Promise((resolve, reject) => {
    entry.file(resolve, reject);
  });
}

function isDirectory(entry: FileSystemEntry): entry is FileSystemDirectoryEntry {
  return entry.isDirectory;
}

function isFile(entry: FileSystemEntry): entry is FileSystemFileEntry {
  return entry.isFile;
}

/** Walks a dropped entry, depth first, keeping the folder structure in the paths. */
async function walk(entry: FileSystemEntry, base: string, out: PickedFile[]): Promise<void> {
  const path = base === '' ? entry.name : `${base}/${entry.name}`;
  if (isFile(entry)) {
    out.push({ file: await fileOf(entry), path });
    return;
  }
  if (isDirectory(entry)) {
    for (const child of await readAllEntries(entry.createReader())) {
      await walk(child, path, out);
    }
  }
}

/**
 * Files of a drop: folders are walked through the File System entries API
 * (webkitGetAsEntry) so that their structure becomes the key prefixes;
 * browsers without it fall back to the flat list of files.
 */
export async function filesFromDataTransfer(dt: DataTransfer): Promise<PickedFile[]> {
  const items = Array.from(dt.items).filter((item) => item.kind === 'file');
  const entries = items
    .map((item) => (typeof item.webkitGetAsEntry === 'function' ? item.webkitGetAsEntry() : null))
    .filter((entry): entry is FileSystemEntry => entry !== null);
  if (entries.length === 0) {
    return filesFromInput(dt.files);
  }
  const out: PickedFile[] = [];
  for (const entry of entries) {
    await walk(entry, '', out);
  }
  return out;
}

/** The object key of a picked file uploaded into a folder of the browser. */
export function keyFor(prefix: string, path: string): string {
  const clean = path
    .split(/[\\/]+/)
    .filter((segment) => segment !== '' && segment !== '.' && segment !== '..')
    .join('/');
  return `${prefix}${clean}`;
}
