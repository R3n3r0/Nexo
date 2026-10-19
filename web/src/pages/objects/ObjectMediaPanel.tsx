import { Alert, Badge, Button, Group, Image, Skeleton, Stack, Table, Text } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconRefresh } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { StorageActions, type PermissionCheck } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { formatNumber } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type MediaState } from '@/services';

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

export interface ObjectMediaPanelProps {
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
  perms: PermissionCheck;
}

/**
 * The outputs of the media worker for a video: the state of its job, the
 * renditions, poster and thumbnails, and the reprocessing of the video
 * (admin:SetBucketMedia).
 */
export function ObjectMediaPanel({ bucket, objectKey, versionId, perms }: ObjectMediaPanelProps) {
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
          {t('objectMedia.source', {
            width: status.width,
            height: status.height,
            duration: clock(status.duration ?? 0),
          })}
        </Text>
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
            alt={t('objectMedia.posterAlt', { name: objectKey.split('/').pop() ?? objectKey })}
            fit="contain"
            mah={240}
            radius="sm"
          />
        </Stack>
      ) : null}
    </Stack>
  );
}
