/** The video types the server packages as HLS (MP4 and its variants). */
const streamableTypes = new Set(['video/mp4', 'video/x-m4v', 'video/quicktime']);

/** Their extensions, for the objects whose content type is unknown (listings). */
const streamableExtension = /\.(mp4|m4v|mov)$/i;

/** Whether a video of this content type plays as an HLS stream. */
export function streamable(contentType: string | undefined): boolean {
  const media = (contentType ?? '').split(';')[0]?.trim().toLowerCase() ?? '';
  return streamableTypes.has(media);
}

/**
 * Whether an object can be shared as an HLS stream: by its content type
 * when known, by its extension otherwise.
 */
export function canStream(key: string, contentType?: string): boolean {
  if (contentType !== undefined && contentType !== '') return streamable(contentType);
  return streamableExtension.test(key);
}

/** Path of the public player page on the S3 listener. */
export const playerPath = '/nexo/media/player/';

/**
 * The public player page of a stream share: the page, served by the S3
 * listener next to the playlists, reads the master playlist from its
 * fragment, which never reaches a server log.
 */
export function playerUrl(masterUrl: string): string {
  const master = new URL(masterUrl);
  return `${master.origin}${playerPath}#${master.pathname}${master.search}`;
}
