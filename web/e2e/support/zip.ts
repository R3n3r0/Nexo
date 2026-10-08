import { inflateRawSync } from 'node:zlib';

/**
 * Reads the entries of a zip archive (stored or deflated, no zip64): the
 * names and contents of the files, keyed by name. Enough to check the
 * archives produced by the console downloads.
 */
export function readZip(archive: Buffer): Map<string, Buffer> {
  // End of central directory: signature 0x06054b50, at most 64 KiB of comment.
  let eocd = -1;
  for (let i = archive.length - 22; i >= Math.max(0, archive.length - 22 - 0xffff); i--) {
    if (archive.readUInt32LE(i) === 0x06054b50) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) {
    throw new Error('not a zip archive');
  }
  const count = archive.readUInt16LE(eocd + 10);
  let offset = archive.readUInt32LE(eocd + 16);
  const entries = new Map<string, Buffer>();
  for (let n = 0; n < count; n++) {
    if (archive.readUInt32LE(offset) !== 0x02014b50) {
      throw new Error('corrupt central directory');
    }
    const method = archive.readUInt16LE(offset + 10);
    const compressed = archive.readUInt32LE(offset + 20);
    const nameLength = archive.readUInt16LE(offset + 28);
    const extraLength = archive.readUInt16LE(offset + 30);
    const commentLength = archive.readUInt16LE(offset + 32);
    const local = archive.readUInt32LE(offset + 42);
    const name = archive.toString('utf8', offset + 46, offset + 46 + nameLength);
    offset += 46 + nameLength + extraLength + commentLength;

    const localName = archive.readUInt16LE(local + 26);
    const localExtra = archive.readUInt16LE(local + 28);
    const start = local + 30 + localName + localExtra;
    const raw = archive.subarray(start, start + compressed);
    if (name.endsWith('/')) {
      continue;
    }
    entries.set(name, method === 0 ? Buffer.from(raw) : inflateRawSync(raw));
  }
  return entries;
}
