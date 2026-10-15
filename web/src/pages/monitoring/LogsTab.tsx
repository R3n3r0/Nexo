import {
  Anchor,
  Badge,
  Group,
  NumberInput,
  Paper,
  SegmentedControl,
  SimpleGrid,
  Stack,
  Table,
  TagsInput,
  Text,
  TextInput,
} from '@mantine/core';
import { useDebouncedValue } from '@mantine/hooks';
import { useCallback, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { formatClock } from '@/lib/monitoring';
import { useServices, type LogEntry, type LogFilter, type StreamHandlers } from '@/services';

import { StreamControls } from './StreamControls';
import { useLiveStream } from './useLiveStream';
import { useNodeNames } from './useNodeNames';
import { VirtualTable } from './VirtualTable';

type Level = NonNullable<LogFilter['level']>;
const levels: readonly Level[] = ['debug', 'info', 'warn', 'error'];

const levelColors: Readonly<Record<string, string>> = {
  DEBUG: 'gray',
  INFO: 'blue',
  WARN: 'yellow',
  ERROR: 'red',
};

/** The attributes of an entry as key=value pairs. */
function attrsText(entry: LogEntry): string {
  return Object.entries(entry.attrs ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join(' ');
}

/**
 * D-MON-02: the live logs of every node (GET /logs, admin:ConsoleLog)
 * with their filters (node, minimum level, entries replayed at the
 * opening), a search of the lines kept, pause and resume, a bounded
 * buffer and the automatic reconnection (TD-U-12).
 */
export function LogsTab() {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { monitoring } = useServices();
  const knownNodes = useNodeNames();
  const [nodes, setNodes] = useState<string[]>([]);
  const [level, setLevel] = useState<Level>('info');
  const [last, setLast] = useState(100);
  const [search, setSearch] = useState('');
  const [limit, setLimit] = useState(1000);
  const [debouncedLast] = useDebouncedValue(last, 500);
  const [debouncedSearch] = useDebouncedValue(search.trim().toLowerCase(), 200);

  const filter = useMemo<LogFilter>(
    () => ({ nodes, level, last: debouncedLast }),
    [nodes, level, debouncedLast],
  );
  const open = useCallback(
    (f: LogFilter, handlers: StreamHandlers<LogEntry>) => monitoring.logs(f, handlers),
    [monitoring],
  );
  const view = useLiveStream(open, filter, { limit });
  const rows =
    debouncedSearch === ''
      ? view.items
      : view.items.filter(
          ({ value }) =>
            value.message.toLowerCase().includes(debouncedSearch) ||
            attrsText(value).toLowerCase().includes(debouncedSearch),
        );
  const nodeOptions = [...new Set([...knownNodes, ...nodes])];

  const columns = [
    { key: 'time', header: t('monitoring.logs.time'), width: 130 },
    { key: 'node', header: t('monitoring.logs.node'), width: 140 },
    { key: 'level', header: t('monitoring.logs.levelColumn'), width: 90 },
    { key: 'message', header: t('monitoring.logs.message') },
  ];

  return (
    <Stack gap="md">
      <Text size="sm" c="dimmed">
        {t('monitoring.logs.hint')}{' '}
        <Anchor component={Link} to="/configuration" size="sm" underline="always">
          {t('nav.configuration')}
        </Anchor>
      </Text>
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('monitoring.logs.filters')}
      >
        <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing="sm">
          <TagsInput
            label={t('monitoring.nodes')}
            placeholder={nodes.length === 0 ? t('monitoring.nodesPlaceholder') : undefined}
            data={nodeOptions}
            value={nodes}
            onChange={setNodes}
            clearable
          />
          <Stack gap={4}>
            <Text size="sm" fw={500} id="logs-level-label">
              {t('monitoring.logs.level')}
            </Text>
            <SegmentedControl
              aria-labelledby="logs-level-label"
              data={levels.map((l) => ({ value: l, label: t(`monitoring.logs.levels.${l}`) }))}
              value={level}
              onChange={(v) => {
                setLevel(v as Level);
              }}
            />
          </Stack>
          <NumberInput
            label={t('monitoring.logs.replay')}
            description={t('monitoring.logs.replayHint')}
            min={0}
            max={10_000}
            step={100}
            value={last}
            onChange={(v) => {
              const n = typeof v === 'number' ? v : Number(v);
              setLast(Number.isFinite(n) ? Math.min(Math.max(Math.trunc(n), 0), 10_000) : 0);
            }}
          />
          <TextInput
            label={t('monitoring.logs.search')}
            placeholder={t('monitoring.logs.searchPlaceholder')}
            type="search"
            value={search}
            onChange={(event) => {
              setSearch(event.currentTarget.value);
            }}
          />
        </SimpleGrid>
      </Paper>
      <StreamControls view={view} limit={limit} onLimitChange={setLimit} />
      <VirtualTable
        label={t('monitoring.logs.table')}
        rows={rows}
        columns={columns}
        rowKey={(row) => row.seq}
        estimate={44}
        minWidth={760}
        empty={view.items.length > 0 ? t('monitoring.logs.noMatch') : t('monitoring.logs.empty')}
        renderRow={({ value: e }) => (
          <>
            <Table.Td style={{ whiteSpace: 'nowrap', verticalAlign: 'top' }}>
              <Text size="sm" ff="monospace">
                {formatClock(e.time, locale)}
              </Text>
            </Table.Td>
            <Table.Td style={{ verticalAlign: 'top' }}>
              <Text size="sm" truncate="end" title={e.node}>
                {e.node}
              </Text>
            </Table.Td>
            <Table.Td style={{ verticalAlign: 'top' }}>
              <Badge color={levelColors[e.level] ?? 'gray'} variant="light">
                {e.level}
              </Badge>
            </Table.Td>
            <Table.Td>
              <Group gap="xs" wrap="wrap">
                <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                  {e.message}
                </Text>
                {e.attrs !== undefined && Object.keys(e.attrs).length > 0 ? (
                  <Text size="xs" c="dimmed" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                    {attrsText(e)}
                  </Text>
                ) : null}
              </Group>
            </Table.Td>
          </>
        )}
      />
    </Stack>
  );
}
