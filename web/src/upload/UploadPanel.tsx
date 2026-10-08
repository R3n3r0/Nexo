import {
  ActionIcon,
  Affix,
  Badge,
  Button,
  Group,
  Paper,
  Progress,
  ScrollArea,
  Stack,
  Text,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import {
  IconChevronDown,
  IconChevronUp,
  IconPlayerPause,
  IconPlayerPlay,
  IconRefresh,
  IconX,
} from '@tabler/icons-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { formatBytes } from '@/lib/format';

import type { UploadItem, UploadStatus } from './manager';
import { useUploadManager, useUploadSnapshot } from './UploadProvider';

const statusColor: Record<UploadStatus, string> = {
  queued: 'gray',
  uploading: 'blue',
  paused: 'yellow',
  completing: 'blue',
  completed: 'green',
  failed: 'red',
  canceled: 'gray',
};

function percent(loaded: number, total: number): number {
  if (total <= 0) {
    return loaded >= total ? 100 : 0;
  }
  return Math.min(100, Math.round((loaded / total) * 100));
}

function UploadRow({ item }: { item: UploadItem }) {
  const { t, i18n } = useTranslation();
  const manager = useUploadManager();
  const locale = i18n.resolvedLanguage;
  const name = item.key;
  const value = item.status === 'completed' ? 100 : percent(item.loaded, item.size);
  const canPause = item.status === 'queued' || item.status === 'uploading';
  const canCancel = canPause || item.status === 'paused' || item.status === 'failed';

  return (
    <Stack gap={4} component="li" style={{ listStyle: 'none' }}>
      <Group justify="space-between" wrap="nowrap" gap="xs">
        <Text size="sm" truncate="start" title={name} style={{ minWidth: 0, flex: 1 }}>
          {name}
        </Text>
        <Group gap={4} wrap="nowrap">
          {canPause ? (
            <Tooltip label={t('uploadQueue.pause')}>
              <ActionIcon
                variant="subtle"
                size="sm"
                aria-label={t('uploadQueue.pauseFile', { name })}
                onClick={() => {
                  manager.pause(item.id);
                }}
              >
                <IconPlayerPause size={14} />
              </ActionIcon>
            </Tooltip>
          ) : null}
          {item.status === 'paused' ? (
            <Tooltip label={t('uploadQueue.resume')}>
              <ActionIcon
                variant="subtle"
                size="sm"
                aria-label={t('uploadQueue.resumeFile', { name })}
                onClick={() => {
                  manager.resume(item.id);
                }}
              >
                <IconPlayerPlay size={14} />
              </ActionIcon>
            </Tooltip>
          ) : null}
          {item.status === 'failed' ? (
            <Tooltip label={t('uploadQueue.retry')}>
              <ActionIcon
                variant="subtle"
                size="sm"
                aria-label={t('uploadQueue.retryFile', { name })}
                onClick={() => {
                  manager.retry(item.id);
                }}
              >
                <IconRefresh size={14} />
              </ActionIcon>
            </Tooltip>
          ) : null}
          {item.status === 'completing' ? (
            // CompleteMultipartUpload is in flight: canceling now could not
            // undo it (the manager refuses too).
            <Tooltip label={t('uploadQueue.cannotCancel')}>
              <span>
                <ActionIcon
                  variant="subtle"
                  color="red"
                  size="sm"
                  disabled
                  aria-label={t('uploadQueue.cancelFile', { name })}
                  aria-describedby={`cannot-cancel-${item.id}`}
                >
                  <IconX size={14} />
                </ActionIcon>
                <VisuallyHidden id={`cannot-cancel-${item.id}`}>
                  {t('uploadQueue.cannotCancel')}
                </VisuallyHidden>
              </span>
            </Tooltip>
          ) : null}
          {canCancel ? (
            <Tooltip label={t('uploadQueue.cancel')}>
              <ActionIcon
                variant="subtle"
                color="red"
                size="sm"
                aria-label={t('uploadQueue.cancelFile', { name })}
                onClick={() => {
                  void manager.cancel(item.id);
                }}
              >
                <IconX size={14} />
              </ActionIcon>
            </Tooltip>
          ) : null}
        </Group>
      </Group>
      <Progress
        value={value}
        size="sm"
        color={statusColor[item.status]}
        animated={item.status === 'uploading'}
        aria-label={t('uploadQueue.progressOf', { name })}
      />
      <Group justify="space-between" gap="xs" wrap="nowrap">
        <Text size="xs" c={item.status === 'failed' ? 'red' : 'dimmed'} truncate>
          {item.status === 'failed' && item.error !== undefined
            ? t('uploadQueue.failedWith', { message: item.error })
            : t(`uploadQueue.status.${item.status}`)}
        </Text>
        <Text size="xs" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
          {t('uploadQueue.bytesOf', {
            loaded: formatBytes(item.status === 'completed' ? item.size : item.loaded, locale),
            total: formatBytes(item.size, locale),
          })}
          {item.multipart
            ? ` · ${t('uploadQueue.parts', { done: item.partsDone, total: item.parts })}`
            : ''}
        </Text>
      </Group>
    </Stack>
  );
}

/**
 * Floating panel with the upload queue: aggregated and per-file progress,
 * pause/resume/cancel/retry. Rendered only while the queue is not empty.
 */
export function UploadPanel() {
  const { t, i18n } = useTranslation();
  const manager = useUploadManager();
  const snapshot = useUploadSnapshot();
  const [collapsed, setCollapsed] = useState(false);
  const locale = i18n.resolvedLanguage;

  if (snapshot.items.length === 0) {
    return null;
  }
  const finished = snapshot.items.length - snapshot.active;
  const total = percent(snapshot.loadedBytes, snapshot.totalBytes);

  return (
    <Affix position={{ bottom: 16, right: 16 }} zIndex={250}>
      <Paper
        withBorder
        shadow="md"
        radius="md"
        p="sm"
        w="min(420px, calc(100vw - 32px))"
        component="section"
        aria-label={t('uploadQueue.panelTitle')}
      >
        <Stack gap="xs">
          <Group justify="space-between" wrap="nowrap">
            <Group gap="xs" wrap="nowrap">
              <Text fw={600} size="sm">
                {t('uploadQueue.panelTitle')}
              </Text>
              <Badge size="sm" variant="light">
                {t('uploadQueue.activeCount', { n: snapshot.active })}
              </Badge>
            </Group>
            <Group gap={4} wrap="nowrap">
              {finished > 0 ? (
                <Button
                  size="compact-xs"
                  variant="subtle"
                  onClick={() => {
                    manager.clearFinished();
                  }}
                >
                  {t('uploadQueue.clearFinished')}
                </Button>
              ) : null}
              <ActionIcon
                variant="subtle"
                size="sm"
                aria-label={collapsed ? t('uploadQueue.expand') : t('uploadQueue.collapse')}
                aria-expanded={!collapsed}
                onClick={() => {
                  setCollapsed((c) => !c);
                }}
              >
                {collapsed ? <IconChevronUp size={16} /> : <IconChevronDown size={16} />}
              </ActionIcon>
            </Group>
          </Group>
          <Progress value={total} size="md" aria-label={t('uploadQueue.totalProgress')} />
          <Text size="xs" c="dimmed" aria-live="polite">
            {t('uploadQueue.bytesOf', {
              loaded: formatBytes(snapshot.loadedBytes, locale),
              total: formatBytes(snapshot.totalBytes, locale),
            })}
          </Text>
          {collapsed ? null : (
            <ScrollArea.Autosize mah={320} type="auto">
              <Stack gap="sm" component="ul" p={0} m={0}>
                {snapshot.items.map((item) => (
                  <UploadRow key={item.id} item={item} />
                ))}
              </Stack>
            </ScrollArea.Autosize>
          )}
        </Stack>
      </Paper>
    </Affix>
  );
}
