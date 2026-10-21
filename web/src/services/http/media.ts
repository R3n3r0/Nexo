import type { MediaService } from '../interfaces';
import type {
  MediaConfiguration,
  MediaStatus,
  SubtitleCues,
  SubtitleTrack,
  Subtitles,
} from '../types';
import type { HttpClient } from './client';

/**
 * MediaService over /buckets/{bucket}/media, /buckets/{bucket}/objects/media/status
 * and /buckets/{bucket}/objects/subtitles.
 */
export function createHttpMediaService(client: HttpClient): MediaService {
  const path = (bucket: string, sub = '') =>
    `/buckets/${encodeURIComponent(bucket)}/media${sub === '' ? '' : `/${sub}`}`;
  const subtitles = (bucket: string, lang?: string) =>
    `/buckets/${encodeURIComponent(bucket)}/objects/subtitles${lang === undefined ? '' : `/${encodeURIComponent(lang)}`}`;

  return {
    get(bucket) {
      return client.request<MediaConfiguration>(path(bucket));
    },
    set(bucket, config) {
      return client.request<MediaConfiguration>(path(bucket), { method: 'PUT', body: config });
    },
    remove(bucket) {
      return client.request<void>(path(bucket), { method: 'DELETE' });
    },
    reprocess(bucket, request) {
      return client.request<void>(path(bucket, 'reprocess'), { method: 'POST', body: request });
    },
    status(bucket, key, versionId) {
      return client.request<MediaStatus>(
        `/buckets/${encodeURIComponent(bucket)}/objects/media/status`,
        { query: { key, versionId } },
      );
    },
    async subtitles(bucket, key, versionId) {
      const out = await client.request<Subtitles>(subtitles(bucket), { query: { key, versionId } });
      return out.tracks;
    },
    subtitleCues(bucket, key, lang, versionId) {
      return client.request<SubtitleCues>(subtitles(bucket, lang), { query: { key, versionId } });
    },
    putSubtitleCues(bucket, key, lang, cues, versionId) {
      return client.request<SubtitleTrack>(subtitles(bucket, lang), {
        method: 'PUT',
        query: { key, versionId },
        body: { cues },
      });
    },
    async uploadSubtitles(bucket, key, lang, document, versionId) {
      const response = await client.send(subtitles(bucket, lang), {
        method: 'PUT',
        query: { key, versionId },
        headers: { 'Content-Type': 'text/vtt' },
        rawBody: document,
      });
      return (await response.json()) as SubtitleTrack;
    },
    deleteSubtitles(bucket, key, lang, versionId) {
      return client.request<void>(subtitles(bucket, lang), {
        method: 'DELETE',
        query: { key, versionId },
      });
    },
    subtitleDownloadUrl(bucket, key, lang, format, versionId) {
      return client.url(subtitles(bucket, lang), { key, versionId, format });
    },
  };
}
