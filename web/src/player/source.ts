/** Paths of the HLS master playlists on the S3 listener. */
const masterPath = /^\/nexo\/media\/v1\/[^/]+\/master\.m3u8$/;

/**
 * The master playlist a player page plays: the path and query in the
 * fragment of the page (/nexo/media/player/#/nexo/media/v1/{bucket}/
 * master.m3u8?key=...), resolved on the origin of the page. Anything else
 * (another origin, another path) is refused: the page plays the streams of
 * its own server only.
 */
export function masterUrl(location: Pick<Location, 'hash' | 'origin'>): URL | undefined {
  const ref = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (!ref.startsWith('/') || ref.startsWith('//')) return undefined;
  let url: URL;
  try {
    url = new URL(ref, location.origin);
  } catch {
    return undefined;
  }
  if (url.origin !== location.origin || !masterPath.test(url.pathname)) return undefined;
  return url;
}
