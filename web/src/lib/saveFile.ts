import { saveBlob } from './download';

/**
 * Files handled in the browser: saving data produced by the console
 * (credentials, configuration export) as a download, without any network
 * request, and reading a file picked by the user.
 */
export function saveTextFile(filename: string, text: string, type = 'text/plain'): void {
  saveBlob(new Blob([text], { type }), filename);
}

/** Saves a value as pretty-printed JSON. */
export function saveJsonFile(filename: string, value: unknown): void {
  saveTextFile(filename, `${JSON.stringify(value, null, 2)}\n`, 'application/json');
}

/** Reads a file picked by the user as text (FileReader: works in every browser and in jsdom). */
export function readTextFile(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      resolve(typeof reader.result === 'string' ? reader.result : '');
    };
    reader.onerror = () => {
      reject(reader.error ?? new Error('The file could not be read.'));
    };
    reader.readAsText(file);
  });
}
