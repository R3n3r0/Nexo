import { Alert, Badge, Button, Group, Select, Text } from '@mantine/core';
import { IconPlayerPause, IconPlayerPlay, IconRefresh, IconTrash } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';

import { errorText } from '@/lib/errorText';
import { bufferSizes } from '@/lib/monitoring';

import type { LiveStreamView } from './useLiveStream';

const stateColors = {
  connecting: 'blue',
  open: 'teal',
  reconnecting: 'yellow',
  closed: 'red',
  paused: 'gray',
} as const;

export interface StreamControlsProps<T> {
  view: LiveStreamView<T>;
  limit: number;
  onLimitChange: (limit: number) => void;
}

/**
 * The state of a live stream and its controls: pause and resume, clear,
 * the size of the buffer; the warnings of a reconnection, of the events
 * the server dropped and of a stream it closed (with a restart).
 */
export function StreamControls<T>({ view, limit, onLimitChange }: StreamControlsProps<T>) {
  const { t } = useTranslation();
  const shown = view.paused && view.state === 'open' ? 'paused' : view.state;
  return (
    <>
      <Group justify="space-between" wrap="wrap" gap="sm">
        <Group gap="sm" wrap="wrap">
          <Badge
            color={stateColors[shown]}
            variant="light"
            size="lg"
            role="status"
            aria-label={`${t('monitoring.stream.status')}: ${t(`monitoring.stream.state.${shown}`)}`}
            data-state={shown}
          >
            {t(`monitoring.stream.state.${shown}`)}
          </Badge>
          <Text size="sm" c="dimmed">
            {t('monitoring.stream.kept', { count: view.items.length })}
          </Text>
          {view.paused && view.pending > 0 ? (
            <Text size="sm" c="dimmed">
              {t('monitoring.stream.pending', { count: view.pending })}
            </Text>
          ) : null}
        </Group>
        <Group gap="xs" wrap="wrap">
          <Select
            aria-label={t('monitoring.stream.buffer')}
            data={bufferSizes.map((n) => ({
              value: String(n),
              label: t('monitoring.stream.bufferOption', { count: n }),
            }))}
            value={String(limit)}
            allowDeselect={false}
            onChange={(v) => {
              if (v !== null) onLimitChange(Number(v));
            }}
            w={140}
          />
          {view.paused ? (
            <Button
              variant="light"
              leftSection={<IconPlayerPlay size={16} />}
              onClick={view.resume}
            >
              {t('monitoring.stream.resume')}
            </Button>
          ) : (
            <Button
              variant="default"
              leftSection={<IconPlayerPause size={16} />}
              onClick={view.pause}
              disabled={view.state === 'closed'}
            >
              {t('monitoring.stream.pause')}
            </Button>
          )}
          <Button variant="default" leftSection={<IconTrash size={16} />} onClick={view.clear}>
            {t('monitoring.stream.clear')}
          </Button>
        </Group>
      </Group>
      {view.state === 'reconnecting' ? (
        <Alert color="yellow" variant="light">
          {t('monitoring.stream.reconnecting')}
        </Alert>
      ) : null}
      {view.dropped > 0 ? (
        <Alert color="yellow" variant="light">
          {t('monitoring.stream.dropped', { count: view.dropped })}
        </Alert>
      ) : null}
      {view.state === 'closed' ? (
        <Alert color="red" variant="light" role="alert">
          <Group justify="space-between" wrap="nowrap" gap="sm">
            <Text size="sm">
              {t('monitoring.stream.stopped', {
                message: errorText(view.error, t('common.unknownError')),
              })}
            </Text>
            <Button
              size="xs"
              variant="default"
              leftSection={<IconRefresh size={14} />}
              onClick={view.restart}
            >
              {t('monitoring.stream.restart')}
            </Button>
          </Group>
        </Alert>
      ) : null}
    </>
  );
}
