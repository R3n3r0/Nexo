/**
 * Object keys in console routes: /buckets/:bucket/browse/<prefix>. Every
 * segment is percent-encoded on its own, so that keys with spaces, "#",
 * "%", "?" or unicode survive the URL and the router, and decoded back
 * from the raw pathname (never from the router params, which apply their
 * own decoding).
 */

/** Encodes a key or prefix for a path, keeping the "/" separators. */
export function encodeKeyPath(key: string): string {
  return key.split('/').map(encodeURIComponent).join('/');
}

/** Decodes a path produced by encodeKeyPath. Malformed escapes are kept verbatim. */
export function decodeKeyPath(path: string): string {
  return path
    .split('/')
    .map((segment) => {
      try {
        return decodeURIComponent(segment);
      } catch {
        return segment;
      }
    })
    .join('/');
}

/** Route of the folder view of a prefix ("" is the root of the bucket). */
export function browsePath(bucket: string, prefix = ''): string {
  return `/buckets/${encodeURIComponent(bucket)}/browse/${encodeKeyPath(prefix)}`;
}

/**
 * The prefix shown by a browse route, read from the raw pathname:
 * "/buckets/b/browse/a%20b/c/" -> "a b/c/".
 */
export function prefixFromPathname(pathname: string, bucket: string): string {
  const marker = `/buckets/${encodeURIComponent(bucket)}/browse`;
  const index = pathname.indexOf(marker);
  if (index === -1) {
    return '';
  }
  const rest = pathname.slice(index + marker.length).replace(/^\//, '');
  return decodeKeyPath(rest);
}

/** Last segment of a key or prefix ("a/b/" -> "b/", "a/b.txt" -> "b.txt"). */
export function baseName(key: string): string {
  const trimmed = key.endsWith('/') ? key.slice(0, -1) : key;
  const slash = trimmed.lastIndexOf('/');
  const name = slash === -1 ? trimmed : trimmed.slice(slash + 1);
  return key.endsWith('/') ? `${name}/` : name;
}

/** Parent prefix of a key or prefix ("a/b/c" -> "a/b/", "a/" -> ""). */
export function parentPrefix(key: string): string {
  const trimmed = key.endsWith('/') ? key.slice(0, -1) : key;
  const slash = trimmed.lastIndexOf('/');
  return slash === -1 ? '' : trimmed.slice(0, slash + 1);
}

/** Breadcrumb segments of a prefix: [{name: "a", prefix: "a/"}, ...]. */
export function prefixSegments(prefix: string): { name: string; prefix: string }[] {
  const out: { name: string; prefix: string }[] = [];
  let acc = '';
  for (const segment of prefix.split('/').slice(0, -1)) {
    acc += `${segment}/`;
    out.push({ name: segment, prefix: acc });
  }
  return out;
}

/** Issue of a folder name typed by the user, or null. */
export function folderNameIssue(name: string): 'required' | 'slash' | 'dots' | null {
  const trimmed = name.replace(/\/+$/, '');
  if (trimmed.trim() === '') return 'required';
  if (trimmed.startsWith('/')) return 'slash';
  if (trimmed.split('/').some((s) => s === '' || s === '.' || s === '..')) return 'dots';
  return null;
}
