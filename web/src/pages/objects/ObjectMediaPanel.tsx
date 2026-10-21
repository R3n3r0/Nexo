import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  CopyButton,
  Group,
  Image,
  Skeleton,
  Stack,
  Table,
  Text,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import {
  IconCheck,
  IconCopy,
  IconExternalLink,
  IconEye,
  IconRefresh,
  IconLayersSubtract,
} from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, type PermissionCheck } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { formatBytes, formatNumber } from '@/lib/format';
import { imageFormatLabels, mediaImageFormats } from '@/lib/mediaRules';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type MediaImageOutput, type MediaState } from '@/services';

import { SubtitlesPanel } from './SubtitlesPanel';

/** How often the state of a video being processed is polled. */
const processingPollMs = 5_000;

const stateColors: Record<MediaState, string> = {
  none: 'gray',
  processing: 'blue',
  done: 'green',
  failed: 'red',
};

/** A duration in seconds as h:mm:ss or m:ss. */
function clock(seconds: number): string {
  const total = Math.max(0, Math.round(seconds));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = String(total % 60).padStart(2, '0');
  return h > 0 ? `${String(h)}:${String(m).padStart(2, '0')}:${s}` : `${String(m)}:${s}`;
}

/** The variants of an image, by format (the most compact first), then by width. */
function sortedVariants(images: readonly MediaImageOutput[]): MediaImageOutput[] {
  const rank = (v: MediaImageOutput) => mediaImageFormats.indexOf(v.format);
  // The image at its own size (width 0) comes after the narrower ones.
  const width = (v: MediaImageOutput) => (v.width === 0 ? Number.MAX_SAFE_INTEGER : v.width);
  return [...images].sort((a, b) => rank(a) - rank(b) || width(a) - width(b));
}

/**
 * The variant previewed first: a format every browser shows (WebP, then
 * JPEG or PNG), at most 640 pixels wide when there is one.
 */
function defaultPreview(variants: readonly MediaImageOutput[]): MediaImageOutput | undefined {
  for (const format of ['webp', 'jpeg', 'png', 'avif'] as const) {
    const ofFormat = variants.filter((v) => v.format === format);
    if (ofFormat.length === 0) continue;
    const small = ofFormat.filter((v) => v.pixelWidth <= 640);
    return small[small.length - 1] ?? ofFormat[0];
  }
  return undefined;
}

const variantId = (v: MediaImageOutput) => `${v.format}-${String(v.width)}`;

/** An absolute URL of a path of the server, for the clipboard. */
function absoluteUrl(url: string): string {
  try {
    return new URL(url, window.location.href).href;
  } catch {
    return url;
  }
}

/**
 * The variants of a processed image: a table by format with their size,
 * the actions to open or copy each one and a preview of the one picked.
 */
function ImageVariants({
  images,
  name,
  served,
}: {
  images: readonly MediaImageOutput[];
  name: string;
  /** The variants derive from the current content (only then are they served). */
  served: boolean;
}) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const variants = sortedVariants(images);
  const [picked, setPicked] = useState<string | undefined>(undefined);
  const preview = served
    ? (variants.find((v) => variantId(v) === picked) ?? defaultPreview(variants))
    : undefined;
  const label = (v: MediaImageOutput) => ({
    format: imageFormatLabels[v.format],
    resolution: `${String(v.pixelWidth)}×${String(v.pixelHeight)}`,
  });

  return (
    <Stack gap="xs">
      <Table.ScrollContainer minWidth={420}>
        <Table aria-label={t('objectMedia.variants')} withTableBorder verticalSpacing={4}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('objectMedia.variantColumns.format')}</Table.Th>
              <Table.Th>{t('objectMedia.variantColumns.width')}</Table.Th>
              <Table.Th>{t('objectMedia.variantColumns.resolution')}</Table.Th>
              <Table.Th>{t('objectMedia.variantColumns.size')}</Table.Th>
              <Table.Th>
                <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                  {t('common.actions')}
                </Text>
              </Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {variants.map((v) => {
              const names = label(v);
              const selected = preview !== undefined && variantId(preview) === variantId(v);
              return (
                <Table.Tr
                  key={variantId(v)}
                  bg={selected ? 'var(--mantine-color-default-hover)' : undefined}
                >
                  <Table.Td>
                    <Badge variant="light" color="teal" tt="none">
                      {names.format}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    {v.width === 0
                      ? t('objectMedia.ownSize')
                      : t('objectMedia.widthPx', { width: v.width })}
                  </Table.Td>
                  <Table.Td>{names.resolution}</Table.Td>
                  <Table.Td>{formatBytes(v.size, locale)}</Table.Td>
                  <Table.Td>
                    {served ? (
                      <Group gap={2} wrap="nowrap" justify="flex-end">
                        <Tooltip label={t('objectMedia.previewVariant', names)}>
                          <ActionIcon
                            variant={selected ? 'light' : 'subtle'}
                            aria-label={t('objectMedia.previewVariant', names)}
                            aria-pressed={selected}
                            onClick={() => {
                              setPicked(variantId(v));
                            }}
                          >
                            <IconEye size={16} />
                          </ActionIcon>
                        </Tooltip>
                        <Tooltip label={t('objectMedia.openVariant', names)}>
                          <ActionIcon
                            variant="subtle"
                            component="a"
                            href={v.url}
                            target="_blank"
                            rel="noopener noreferrer"
                            aria-label={t('objectMedia.openVariant', names)}
                          >
                            <IconExternalLink size={16} />
                          </ActionIcon>
                        </Tooltip>
                        <CopyButton value={absoluteUrl(v.url)}>
                          {({ copied, copy }) => (
                            <Tooltip
                              label={
                                copied ? t('common.copied') : t('objectMedia.copyVariant', names)
                              }
                            >
                              <ActionIcon
                                variant="subtle"
                                aria-label={t('objectMedia.copyVariant', names)}
                                onClick={copy}
                              >
                                {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                              </ActionIcon>
                            </Tooltip>
                          )}
                        </CopyButton>
                      </Group>
                    ) : null}
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      {served ? (
        <Text size="xs" c="dimmed">
          {t('objectMedia.variantsHint')}
        </Text>
      ) : null}
      {preview !== undefined ? (
        <Stack gap={4}>
          <Text size="sm" fw={500}>
            {t('objectMedia.preview')}
          </Text>
          <Image
            src={preview.url}
            alt={t('objectMedia.variantAlt', { ...label(preview), name })}
            fit="contain"
            mah={240}
            radius="sm"
            style={{
              // A checkerboard shows the transparency of the image.
              backgroundImage:
                'repeating-conic-gradient(var(--mantine-color-default-border) 0 25%, transparent 0 50%)',
              backgroundSize: '16px 16px',
            }}
          />
        </Stack>
      ) : null}
    </Stack>
  );
}

export interface ObjectMediaPanelProps {
  /** A video (renditions, poster, thumbnails) or an image (variants). */
  kind?: 'video' | 'image';
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
  perms: PermissionCheck;
}

/**
 * The outputs of the media worker for a video or an image: the state of
 * its job, the renditions, poster and thumbnails of a video or the
 * variants of an image, and the reprocessing (admin:SetBucketMedia).
 */
export function ObjectMediaPanel({
  kind = 'video',
  bucket,
  objectKey,
  versionId,
  perms,
}: ObjectMediaPanelProps) {
  const { t, i18n } = useTranslation();
  const { media, objects } = useServices();
  const queryClient = useQueryClient();
  const locale = i18n.resolvedLanguage;
  const key = storageKeys.mediaStatus(bucket, objectKey, versionId);
  const query = useQuery({
    queryKey: key,
    queryFn: () => media.status(bucket, objectKey, versionId),
    refetchInterval: (q) => (q.state.data?.state === 'processing' ? processingPollMs : false),
  });
  const reprocess = useMutation({
    mutationFn: () =>
      media.reprocess(bucket, {
        key: objectKey,
        ...(versionId !== undefined ? { versionId } : {}),
      }),
    onSuccess: async () => {
      notifications.show({ color: 'green', message: t('objectMedia.queued') });
      await queryClient.invalidateQueries({ queryKey: key });
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });
  const canReprocess = perms.can(StorageActions.setMedia);

  if (query.isPending) {
    return <Skeleton height={120} />;
  }
  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {problemMessage(t, query.error)}
      </Alert>
    );
  }
  const status = query.data;
  const stale = status.state !== 'none' && !status.current;
  const poster =
    status.poster === true && status.current
      ? objects.posterUrl(bucket, objectKey, versionId)
      : undefined;
  const renditions = status.renditions ?? [];
  const images = status.images ?? [];
  const isImage = kind === 'image' || images.length > 0;
  const name = objectKey.split('/').pop() ?? objectKey;

  return (
    <Stack gap="sm">
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Group gap="xs">
          <Badge variant="light" color={stateColors[status.state]}>
            {t(`objectMedia.state.${status.state}`)}
          </Badge>
          {stale ? (
            <Badge variant="outline" color="orange">
              {t('objectMedia.stale')}
            </Badge>
          ) : null}
          {status.alpha === true ? (
            <Badge
              variant="light"
              color="violet"
              tt="none"
              leftSection={<IconLayersSubtract size={12} aria-hidden />}
            >
              {t('objectMedia.alpha')}
            </Badge>
          ) : null}
          {status.rule !== undefined && status.rule !== '' ? (
            <Text size="xs" c="dimmed">
              {t('objectMedia.rule', { rule: status.rule })}
            </Text>
          ) : null}
        </Group>
        <PermissionGuard allowed={canReprocess} action={StorageActions.setMedia}>
          {(disabled) => (
            <Button
              size="xs"
              variant="default"
              leftSection={<IconRefresh size={14} />}
              disabled={disabled}
              loading={reprocess.isPending}
              onClick={() => {
                reprocess.mutate();
              }}
            >
              {t('objectMedia.reprocess')}
            </Button>
          )}
        </PermissionGuard>
      </Group>

      {status.state === 'none' ? (
        <Text size="sm" c="dimmed">
          {t('objectMedia.none')}
        </Text>
      ) : null}
      {status.state === 'processing' && status.step !== undefined && status.step !== '' ? (
        <Text size="sm" role="status">
          {t('objectMedia.step', { step: status.step })}
        </Text>
      ) : null}
      {status.state === 'failed' && status.error !== undefined && status.error !== '' ? (
        <Alert color="red" variant="light" role="alert" title={t('objectMedia.failed')}>
          <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
            {status.error}
          </Text>
        </Alert>
      ) : null}
      {stale ? (
        <Text size="xs" c="dimmed">
          {t('objectMedia.staleHint')}
        </Text>
      ) : null}

      {status.width !== undefined && status.height !== undefined ? (
        <Text size="sm">
          {isImage
            ? t('objectMedia.imageSource', { width: status.width, height: status.height })
            : t('objectMedia.source', {
                width: status.width,
                height: status.height,
                duration: clock(status.duration ?? 0),
              })}
        </Text>
      ) : null}
      {status.alpha === true ? (
        <Text size="xs" c="dimmed">
          {t('objectMedia.alphaHint')}
        </Text>
      ) : null}

      {images.length > 0 ? (
        <ImageVariants
          images={images}
          name={name}
          served={status.state === 'done' && status.current}
        />
      ) : null}

      {renditions.length > 0 ? (
        <Table aria-label={t('objectMedia.renditions')} withTableBorder verticalSpacing={4}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('objectMedia.columns.name')}</Table.Th>
              <Table.Th>{t('objectMedia.columns.resolution')}</Table.Th>
              <Table.Th>{t('objectMedia.columns.bandwidth')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {renditions.map((r) => (
              <Table.Tr key={r.name}>
                <Table.Td>{r.name}</Table.Td>
                <Table.Td>
                  {r.width}×{r.height}
                </Table.Td>
                <Table.Td>
                  {t('objectMedia.mbps', {
                    value: formatNumber(Math.round(r.bandwidth / 100_000) / 10, locale),
                  })}
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      ) : null}

      {status.thumbnails !== undefined ? (
        <Text size="sm">
          {t('objectMedia.thumbnails', {
            n: status.thumbnails.count,
            interval: status.thumbnails.interval,
          })}
        </Text>
      ) : null}

      {poster !== undefined ? (
        <Stack gap={4}>
          <Text size="sm" fw={500}>
            {t('objectMedia.poster')}
          </Text>
          <Image
            src={poster}
            alt={t('objectMedia.posterAlt', { name })}
            fit="contain"
            mah={240}
            radius="sm"
          />
        </Stack>
      ) : null}

      {!isImage ? (
        <SubtitlesPanel
          bucket={bucket}
          objectKey={objectKey}
          versionId={versionId}
          perms={perms}
          automaticError={status.subtitlesError}
        />
      ) : null}
    </Stack>
  );
}
