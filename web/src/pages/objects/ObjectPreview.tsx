import {
  Alert,
  Anchor,
  Box,
  Code,
  Group,
  Image,
  ScrollArea,
  Skeleton,
  Stack,
  Text,
} from '@mantine/core';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { formatBytes } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices } from '@/services';

/** How an object can be previewed in the console (mirrors the server's safe types). */
export type PreviewKind = 'image' | 'pdf' | 'text' | 'json' | 'audio' | 'video' | 'none';

/** Bytes of a text object loaded by the preview. */
export const previewTextLimit = 256 * 1024;

const imageTypes = new Set([
  'image/png',
  'image/jpeg',
  'image/gif',
  'image/webp',
  'image/avif',
  'image/bmp',
  'image/x-icon',
  'image/vnd.microsoft.icon',
]);

/**
 * The preview of a content type. SVG, HTML and other active types are
 * never rendered: the server serves them as attachments anyway.
 */
// eslint-disable-next-line react-refresh/only-export-components -- pure helper tested with the component
export function previewKind(contentType: string | undefined): PreviewKind {
  const media = (contentType ?? '').split(';')[0]?.trim().toLowerCase() ?? '';
  if (imageTypes.has(media)) return 'image';
  if (media === 'application/pdf') return 'pdf';
  if (media === 'text/plain') return 'text';
  if (media === 'application/json') return 'json';
  if (media.startsWith('audio/')) return 'audio';
  if (media.startsWith('video/')) return 'video';
  return 'none';
}

/** Indents JSON text; returns it unchanged when it does not parse. */
function prettyJson(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text) as unknown, null, 2);
  } catch {
    return text;
  }
}

function TextPreview({
  bucket,
  objectKey,
  versionId,
  json,
}: {
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
  json: boolean;
}) {
  const { t, i18n } = useTranslation();
  const { objects } = useServices();
  const query = useQuery({
    queryKey: storageKeys.preview(bucket, objectKey, versionId),
    queryFn: ({ signal }) =>
      objects.previewText(bucket, objectKey, {
        maxBytes: previewTextLimit,
        signal,
        ...(versionId !== undefined ? { versionId } : {}),
      }),
    staleTime: 60_000,
  });
  if (query.isPending) {
    return <Skeleton height={200} />;
  }
  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {problemMessage(t, query.error)}
      </Alert>
    );
  }
  const { text, truncated } = query.data;
  return (
    <Stack gap="xs">
      {truncated ? (
        <Text size="xs" c="dimmed" role="status">
          {t('objectPreview.truncated', {
            size: formatBytes(previewTextLimit, i18n.resolvedLanguage),
          })}
        </Text>
      ) : null}
      <ScrollArea.Autosize mah={480} type="auto">
        <Code block style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
          {json && !truncated ? prettyJson(text) : text}
        </Code>
      </ScrollArea.Autosize>
    </Stack>
  );
}

export interface ObjectPreviewProps {
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
  contentType?: string | undefined;
  size: number;
}

/**
 * D-OBJ-05: images, PDF, text and JSON (size-limited), audio and video.
 *
 * The PDF frame is not sandboxed: Chromium refuses to start its PDF viewer
 * in a sandboxed frame. The preview endpoint serves only application/pdf
 * inline to it (nosniff, so another document typed as PDF never renders as
 * HTML) and the viewer runs in its own context, not in the console origin.
 */
export function ObjectPreview({
  bucket,
  objectKey,
  versionId,
  contentType,
  size,
}: ObjectPreviewProps) {
  const { t } = useTranslation();
  const { objects } = useServices();
  const kind = previewKind(contentType);
  const url = objects.previewUrl(bucket, objectKey, versionId);
  const name = objectKey.split('/').pop() ?? objectKey;

  switch (kind) {
    case 'image':
      return (
        <Box>
          <Image
            src={url}
            alt={t('objectPreview.imageAlt', { name })}
            fit="contain"
            mah={480}
            radius="sm"
          />
        </Box>
      );
    case 'pdf':
      return (
        <Stack gap="xs">
          <iframe
            src={url}
            title={t('objectPreview.pdfTitle', { name })}
            style={{ width: '100%', height: 480, border: 0 }}
          />
          <Group>
            <Anchor href={url} target="_blank" rel="noopener noreferrer" size="sm">
              {t('objectPreview.openInTab')}
            </Anchor>
          </Group>
        </Stack>
      );
    case 'audio':
      return (
        <audio
          controls
          src={url}
          style={{ width: '100%' }}
          aria-label={t('objectPreview.audioLabel', { name })}
        >
          <track kind="captions" />
        </audio>
      );
    case 'video':
      return (
        <video
          controls
          src={url}
          style={{ width: '100%', maxHeight: 480 }}
          aria-label={t('objectPreview.videoLabel', { name })}
        >
          <track kind="captions" />
        </video>
      );
    case 'text':
    case 'json':
      if (size === 0) {
        return <Text size="sm">{t('objectPreview.empty')}</Text>;
      }
      return (
        <TextPreview
          bucket={bucket}
          objectKey={objectKey}
          versionId={versionId}
          json={kind === 'json'}
        />
      );
    default:
      return (
        <Text size="sm" c="dimmed">
          {t('objectPreview.unavailable', { type: contentType ?? 'application/octet-stream' })}
        </Text>
      );
  }
}
