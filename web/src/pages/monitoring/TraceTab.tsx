import {
  Anchor,
  Badge,
  CloseButton,
  Group,
  MultiSelect,
  Paper,
  SimpleGrid,
  Stack,
  Switch,
  Table,
  TagsInput,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { useDebouncedValue } from '@mantine/hooks';
import { useCallback, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { formatBytes } from '@/lib/format';
import {
  formatClock,
  formatMs,
  isStatusFilter,
  parseGoDuration,
  statusColor,
} from '@/lib/monitoring';
import {
  useServices,
  type StreamHandlers,
  type TraceEvent,
  type TraceFilter,
  type TraceType,
} from '@/services';

import { StreamControls } from './StreamControls';
import { useLiveStream, type LiveEntry } from './useLiveStream';
import { useNodeNames } from './useNodeNames';
import { VirtualTable } from './VirtualTable';

const traceTypes: readonly TraceType[] = ['s3', 'admin', 'web', 'rpc'];

/** Operations offered as suggestions (any name the server reports is accepted). */
const apiSuggestions = [
  'PutObject',
  'GetObject',
  'HeadObject',
  'DeleteObject',
  'DeleteObjects',
  'ListObjectsV2',
  'ListBuckets',
  'CreateBucket',
  'DeleteBucket',
  'CompleteMultipartUpload',
  'UploadPart',
  'CopyObject',
];

const statusSuggestions = ['2xx', '3xx', '4xx', '5xx', '403', '404', '500', '503'];

/** The details of one request of the trace. */
function TraceDetails({ event, onClose }: { event: TraceEvent; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const rows: [string, string | undefined][] = [
    [t('monitoring.trace.time'), new Date(event.time).toLocaleString(locale)],
    [t('monitoring.trace.node'), event.node],
    [t('monitoring.trace.api'), event.api],
    [t('monitoring.trace.request'), `${event.method} ${event.path}`],
    [t('monitoring.trace.query'), event.query],
    [t('monitoring.trace.status'), String(event.status)],
    [t('monitoring.trace.duration'), formatMs(event.durationMs, locale)],
    [
      t('monitoring.trace.bytes'),
      `${formatBytes(event.inputBytes, locale)} / ${formatBytes(event.outputBytes, locale)}`,
    ],
    [t('monitoring.trace.bucket'), event.bucket],
    [t('monitoring.trace.user'), event.user],
    [t('monitoring.trace.client'), event.clientIp],
    [t('monitoring.trace.requestId'), event.requestId],
    [t('monitoring.trace.error'), event.error],
  ];
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('monitoring.stream.details')}
    >
      <Group justify="space-between" mb="xs">
        <Title order={4}>{t('monitoring.stream.details')}</Title>
        <CloseButton aria-label={t('monitoring.stream.closeDetails')} onClick={onClose} />
      </Group>
      <Table variant="vertical" layout="fixed" withTableBorder={false}>
        <Table.Tbody>
          {rows
            .filter(([, value]) => value !== undefined && value !== '')
            .map(([label, value]) => (
              <Table.Tr key={label}>
                <Table.Th w={160}>{label}</Table.Th>
                <Table.Td style={{ overflowWrap: 'anywhere' }}>
                  <Text size="sm" ff="monospace">
                    {value}
                  </Text>
                </Table.Td>
              </Table.Tr>
            ))}
        </Table.Tbody>
      </Table>
    </Paper>
  );
}

/**
 * D-MON-02: the live trace of the requests of every node (GET /trace,
 * admin:ServerTrace) with its filters (node, type, API, bucket, status,
 * errors only, minimum duration), pause and resume, a bounded buffer and
 * the automatic reconnection (TD-U-12, TD-E-16).
 */
export function TraceTab() {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { monitoring } = useServices();
  const knownNodes = useNodeNames();
  const [nodes, setNodes] = useState<string[]>([]);
  const [types, setTypes] = useState<TraceType[]>([]);
  const [apis, setApis] = useState<string[]>([]);
  const [buckets, setBuckets] = useState<string[]>([]);
  const [statuses, setStatuses] = useState<string[]>([]);
  const [statusError, setStatusError] = useState(false);
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [threshold, setThreshold] = useState('');
  const [limit, setLimit] = useState(1000);
  const [selected, setSelected] = useState<LiveEntry<TraceEvent> | null>(null);
  const [debouncedThreshold] = useDebouncedValue(threshold.trim(), 400);
  const thresholdInvalid = threshold.trim() !== '' && parseGoDuration(threshold) === undefined;
  const appliedThreshold =
    debouncedThreshold !== '' && parseGoDuration(debouncedThreshold) !== undefined
      ? debouncedThreshold
      : undefined;

  const filter = useMemo<TraceFilter>(
    () => ({
      nodes,
      types,
      apis,
      buckets,
      statuses,
      errorsOnly,
      ...(appliedThreshold !== undefined ? { threshold: appliedThreshold } : {}),
    }),
    [nodes, types, apis, buckets, statuses, errorsOnly, appliedThreshold],
  );
  const open = useCallback(
    (f: TraceFilter, handlers: StreamHandlers<TraceEvent>) => monitoring.trace(f, handlers),
    [monitoring],
  );
  const view = useLiveStream(open, filter, { limit });
  const nodeOptions = [...new Set([...knownNodes, ...nodes])];

  const columns = [
    { key: 'time', header: t('monitoring.trace.time'), width: 130 },
    { key: 'node', header: t('monitoring.trace.node'), width: 140 },
    { key: 'api', header: t('monitoring.trace.api'), width: 170 },
    { key: 'request', header: t('monitoring.trace.request') },
    { key: 'status', header: t('monitoring.trace.status'), width: 90 },
    { key: 'duration', header: t('monitoring.trace.duration'), width: 100 },
    { key: 'bytes', header: t('monitoring.trace.bytes'), width: 150 },
    { key: 'user', header: t('monitoring.trace.user'), width: 130 },
  ];

  return (
    <Stack gap="md">
      <Text size="sm" c="dimmed">
        {t('monitoring.trace.hint')}
      </Text>
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('monitoring.trace.filters')}
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
          <MultiSelect
            label={t('monitoring.trace.types')}
            placeholder={types.length === 0 ? t('monitoring.trace.typesPlaceholder') : undefined}
            data={traceTypes.map((v) => ({ value: v, label: t(`monitoring.trace.type.${v}`) }))}
            value={types}
            onChange={(v) => {
              setTypes(v as TraceType[]);
            }}
            clearable
          />
          <TagsInput
            label={t('monitoring.trace.apis')}
            placeholder={apis.length === 0 ? t('monitoring.trace.apisPlaceholder') : undefined}
            data={apiSuggestions}
            value={apis}
            onChange={setApis}
            clearable
          />
          <TagsInput
            label={t('monitoring.trace.buckets')}
            placeholder={
              buckets.length === 0 ? t('monitoring.trace.bucketsPlaceholder') : undefined
            }
            value={buckets}
            onChange={setBuckets}
            clearable
          />
          <TagsInput
            label={t('monitoring.trace.statuses')}
            placeholder={
              statuses.length === 0 ? t('monitoring.trace.statusesPlaceholder') : undefined
            }
            data={statusSuggestions}
            value={statuses}
            error={statusError ? t('monitoring.trace.statusInvalid') : undefined}
            onChange={(values) => {
              const valid = values.filter(isStatusFilter).map((v) => v.toLowerCase());
              setStatusError(valid.length !== values.length);
              setStatuses(valid);
            }}
            clearable
          />
          <TextInput
            label={t('monitoring.trace.minDuration')}
            placeholder={t('monitoring.trace.minDurationPlaceholder')}
            value={threshold}
            error={thresholdInvalid ? t('monitoring.trace.minDurationInvalid') : undefined}
            onChange={(event) => {
              setThreshold(event.currentTarget.value);
            }}
          />
          <Switch
            label={t('monitoring.trace.errorsOnly')}
            checked={errorsOnly}
            onChange={(event) => {
              setErrorsOnly(event.currentTarget.checked);
            }}
            mt={{ base: 0, sm: 'xl' }}
          />
        </SimpleGrid>
      </Paper>
      <StreamControls view={view} limit={limit} onLimitChange={setLimit} />
      <VirtualTable
        label={t('monitoring.trace.table')}
        rows={view.items}
        columns={columns}
        rowKey={(row) => row.seq}
        empty={t('monitoring.trace.empty')}
        renderRow={({ seq, value: e }) => (
          <>
            <Table.Td style={{ whiteSpace: 'nowrap' }}>
              <Anchor
                component="button"
                type="button"
                size="sm"
                ff="monospace"
                aria-label={t('monitoring.trace.detailsOf', {
                  api: e.api ?? e.path,
                  time: formatClock(e.time, locale),
                })}
                onClick={() => {
                  setSelected({ seq, value: e });
                }}
              >
                {formatClock(e.time, locale)}
              </Anchor>
            </Table.Td>
            <Table.Td>
              <Text size="sm" truncate="end" title={e.node}>
                {e.node}
              </Text>
            </Table.Td>
            <Table.Td>
              <Group gap={4} wrap="nowrap">
                {e.type !== 's3' ? (
                  <Badge size="xs" variant="outline" color="gray">
                    {t(`monitoring.trace.type.${e.type}`)}
                  </Badge>
                ) : null}
                <Text size="sm" truncate="end" title={e.api}>
                  {e.api ?? '—'}
                </Text>
              </Group>
            </Table.Td>
            <Table.Td>
              <Text
                size="sm"
                ff="monospace"
                truncate="end"
                title={`${e.method} ${e.path}${e.query ? `?${e.query}` : ''}`}
              >
                {e.method} {e.path}
                {e.query ? `?${e.query}` : ''}
              </Text>
            </Table.Td>
            <Table.Td>
              <Badge color={statusColor(e.status)} variant="light" title={e.error}>
                {e.status}
              </Badge>
            </Table.Td>
            <Table.Td style={{ whiteSpace: 'nowrap' }}>{formatMs(e.durationMs, locale)}</Table.Td>
            <Table.Td style={{ whiteSpace: 'nowrap' }}>
              {formatBytes(e.inputBytes, locale)} / {formatBytes(e.outputBytes, locale)}
            </Table.Td>
            <Table.Td>
              <Text size="sm" truncate="end" title={e.user}>
                {e.user ?? '—'}
              </Text>
            </Table.Td>
          </>
        )}
      />
      {selected !== null ? (
        <TraceDetails
          event={selected.value}
          onClose={() => {
            setSelected(null);
          }}
        />
      ) : null}
    </Stack>
  );
}
