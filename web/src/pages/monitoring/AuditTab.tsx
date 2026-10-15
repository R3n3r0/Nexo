import {
  ActionIcon,
  Badge,
  Button,
  Group,
  Paper,
  Select,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  TagsInput,
  Text,
  TextInput,
} from '@mantine/core';
import { IconChevronDown, IconChevronRight, IconDownload, IconSearch } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { Fragment, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { ScrollTable } from '@/components/cache/ScrollTable';
import { ErrorAlert } from '@/components/iam/common';
import { formatBytes } from '@/lib/format';
import { formatMs, statusColor } from '@/lib/monitoring';
import { saveJsonFile } from '@/lib/saveFile';
import { useServices, type AuditEntry, type AuditQuery } from '@/services';

import { useNodeNames } from './useNodeNames';

const outcomes = ['any', 'success', 'failed', 'denied'] as const;
type Outcome = (typeof outcomes)[number];
const types = ['any', 's3', 'admin'] as const;
type ApiType = (typeof types)[number];
const periods = ['15m', '1h', '24h', '7d', 'all', 'custom'] as const;
type Period = (typeof periods)[number];
const periodMs: Readonly<Record<Exclude<Period, 'all' | 'custom'>, number>> = {
  '15m': 15 * 60_000,
  '1h': 3_600_000,
  '24h': 86_400_000,
  '7d': 7 * 86_400_000,
};
const limits = [100, 500, 1000] as const;

/** The search form of the audit log. */
interface AuditForm {
  user: string;
  api: string;
  bucket: string;
  outcome: Outcome;
  type: ApiType;
  period: Period;
  since: string;
  until: string;
  requestId: string;
  nodes: string[];
  limit: number;
}

const emptyForm: AuditForm = {
  user: '',
  api: '',
  bucket: '',
  outcome: 'any',
  type: 'any',
  period: '24h',
  since: '',
  until: '',
  requestId: '',
  nodes: [],
  limit: 100,
};

/** A datetime-local value as RFC 3339 (undefined when empty or invalid). */
function localToIso(value: string): string | undefined {
  if (value === '') return undefined;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

/** The query of GET /audit for a form, the period counted back from now. */
function auditQueryOf(form: AuditForm, now: Date): AuditQuery {
  const query: AuditQuery = { limit: form.limit };
  if (form.user.trim() !== '') query.user = form.user.trim();
  if (form.api.trim() !== '') query.api = form.api.trim();
  if (form.bucket.trim() !== '') query.bucket = form.bucket.trim();
  if (form.requestId.trim() !== '') query.requestId = form.requestId.trim();
  if (form.nodes.length > 0) query.nodes = form.nodes;
  if (form.type !== 'any') query.type = form.type;
  if (form.outcome === 'success') query.statuses = ['1xx', '2xx', '3xx'];
  if (form.outcome === 'failed') query.errorsOnly = true;
  if (form.outcome === 'denied') query.statuses = ['403'];
  if (form.period === 'custom') {
    const since = localToIso(form.since);
    const until = localToIso(form.until);
    if (since !== undefined) query.since = since;
    if (until !== undefined) query.until = until;
  } else if (form.period !== 'all') {
    query.since = new Date(now.getTime() - periodMs[form.period]).toISOString();
  }
  return query;
}

/** The details of a record (the row under it). */
function AuditDetails({ entry }: { entry: AuditEntry }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const rows: [string, string | undefined][] = [
    [t('monitoring.audit.details.requestId'), entry.requestId],
    [
      t('monitoring.audit.details.method'),
      entry.method !== undefined ? `${entry.method} ${entry.path ?? ''}` : undefined,
    ],
    [t('monitoring.audit.details.parentUser'), entry.parentUser],
    [t('monitoring.audit.details.userAgent'), entry.userAgent],
    [
      t('monitoring.audit.details.bytes'),
      `${formatBytes(entry.rx, locale)} / ${formatBytes(entry.tx, locale)}`,
    ],
    [
      t('monitoring.audit.details.query'),
      entry.query !== undefined && Object.keys(entry.query).length > 0
        ? Object.entries(entry.query)
            .map(([k, v]) => `${k}=${v}`)
            .join('&')
        : undefined,
    ],
    [t('monitoring.audit.details.error'), entry.error],
  ];
  return (
    <Table variant="vertical" layout="fixed" withTableBorder={false}>
      <Table.Tbody>
        {rows
          .filter(([, v]) => v !== undefined && v !== '')
          .map(([label, value]) => (
            <Table.Tr key={label}>
              <Table.Th w={180}>{label}</Table.Th>
              <Table.Td style={{ overflowWrap: 'anywhere' }}>
                <Text size="sm" ff="monospace">
                  {value}
                </Text>
              </Table.Td>
            </Table.Tr>
          ))}
      </Table.Tbody>
    </Table>
  );
}

/**
 * D-MON-03: the search of the audit log (GET /audit, admin:ConsoleLog)
 * by user, action, bucket, outcome, API, period, node and request id,
 * aggregated over the nodes, newest first, with the details of every
 * record and the download of the results.
 */
export function AuditTab() {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { monitoring } = useServices();
  const knownNodes = useNodeNames();
  const [form, setForm] = useState<AuditForm>(emptyForm);
  const [applied, setApplied] = useState<AuditQuery>(() => auditQueryOf(emptyForm, new Date()));
  const [expanded, setExpanded] = useState<ReadonlySet<number>>(new Set());
  const query = useQuery({
    queryKey: ['monitoring', 'audit', applied],
    queryFn: () => monitoring.audit(applied),
  });
  const set = <K extends keyof AuditForm>(key: K, value: AuditForm[K]) => {
    setForm((f) => ({ ...f, [key]: value }));
  };
  const submit = () => {
    setExpanded(new Set());
    setApplied(auditQueryOf(form, new Date()));
  };
  const items = query.data ?? [];

  return (
    <Stack gap="md">
      <Text size="sm" c="dimmed">
        {t('monitoring.audit.hint')}
      </Text>
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('monitoring.audit.form')}
      >
        <form
          noValidate
          onSubmit={(event) => {
            event.preventDefault();
            submit();
          }}
        >
          <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing="sm">
            <TextInput
              label={t('monitoring.audit.user')}
              placeholder={t('monitoring.audit.userPlaceholder')}
              value={form.user}
              onChange={(e) => {
                set('user', e.currentTarget.value);
              }}
            />
            <TextInput
              label={t('monitoring.audit.api')}
              placeholder={t('monitoring.audit.apiPlaceholder')}
              value={form.api}
              onChange={(e) => {
                set('api', e.currentTarget.value);
              }}
            />
            <TextInput
              label={t('monitoring.audit.bucket')}
              value={form.bucket}
              onChange={(e) => {
                set('bucket', e.currentTarget.value);
              }}
            />
            <Select
              label={t('monitoring.audit.outcome')}
              data={outcomes.map((o) => ({ value: o, label: t(`monitoring.audit.outcomes.${o}`) }))}
              value={form.outcome}
              allowDeselect={false}
              onChange={(v) => {
                set('outcome', (v ?? 'any') as Outcome);
              }}
            />
            <Select
              label={t('monitoring.audit.type')}
              data={types.map((o) => ({ value: o, label: t(`monitoring.audit.types.${o}`) }))}
              value={form.type}
              allowDeselect={false}
              onChange={(v) => {
                set('type', (v ?? 'any') as ApiType);
              }}
            />
            <Select
              label={t('monitoring.audit.period')}
              data={periods.map((p) => ({ value: p, label: t(`monitoring.audit.periods.${p}`) }))}
              value={form.period}
              allowDeselect={false}
              onChange={(v) => {
                set('period', (v ?? '24h') as Period);
              }}
            />
            {form.period === 'custom' ? (
              <>
                <TextInput
                  type="datetime-local"
                  label={t('monitoring.audit.since')}
                  value={form.since}
                  onChange={(e) => {
                    set('since', e.currentTarget.value);
                  }}
                />
                <TextInput
                  type="datetime-local"
                  label={t('monitoring.audit.until')}
                  value={form.until}
                  onChange={(e) => {
                    set('until', e.currentTarget.value);
                  }}
                />
              </>
            ) : null}
            <TextInput
              label={t('monitoring.audit.requestId')}
              value={form.requestId}
              onChange={(e) => {
                set('requestId', e.currentTarget.value);
              }}
            />
            <TagsInput
              label={t('monitoring.nodes')}
              placeholder={form.nodes.length === 0 ? t('monitoring.nodesPlaceholder') : undefined}
              data={[...new Set([...knownNodes, ...form.nodes])]}
              value={form.nodes}
              onChange={(v) => {
                set('nodes', v);
              }}
              clearable
            />
            <Select
              label={t('monitoring.audit.limit')}
              data={limits.map((n) => ({
                value: String(n),
                label: t('monitoring.audit.limitOption', { count: n }),
              }))}
              value={String(form.limit)}
              allowDeselect={false}
              onChange={(v) => {
                set('limit', Number(v ?? 100));
              }}
            />
          </SimpleGrid>
          <Group justify="flex-end" mt="md" gap="xs">
            <Button
              variant="default"
              onClick={() => {
                setForm(emptyForm);
              }}
            >
              {t('monitoring.audit.reset')}
            </Button>
            <Button type="submit" leftSection={<IconSearch size={16} />} loading={query.isFetching}>
              {t('monitoring.audit.search')}
            </Button>
          </Group>
        </form>
      </Paper>
      {query.isError ? (
        <ErrorAlert
          error={query.error}
          title={t('monitoring.audit.loadError')}
          onRetry={() => void query.refetch()}
        />
      ) : null}
      {query.isPending ? <Skeleton height={240} /> : null}
      {query.data !== undefined ? (
        <Paper
          withBorder
          radius="md"
          p="md"
          component="section"
          aria-label={t('monitoring.audit.table')}
        >
          <Group justify="space-between" mb="sm" wrap="wrap">
            <Text size="sm" c="dimmed" role="status">
              {t('monitoring.audit.results', { count: items.length })}
            </Text>
            <Button
              variant="default"
              size="xs"
              leftSection={<IconDownload size={14} />}
              disabled={items.length === 0}
              onClick={() => {
                saveJsonFile('nexo-audit.json', items);
              }}
            >
              {t('monitoring.audit.export')}
            </Button>
          </Group>
          {items.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t('monitoring.audit.empty')}
            </Text>
          ) : (
            <ScrollTable minWidth={960} label={t('monitoring.audit.table')}>
              <Table highlightOnHover aria-label={t('monitoring.audit.table')} verticalSpacing={6}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th w={40} />
                    <Table.Th>{t('monitoring.audit.time')}</Table.Th>
                    <Table.Th>{t('monitoring.audit.who')}</Table.Th>
                    <Table.Th>{t('monitoring.audit.action')}</Table.Th>
                    <Table.Th>{t('monitoring.audit.resource')}</Table.Th>
                    <Table.Th>{t('monitoring.audit.status')}</Table.Th>
                    <Table.Th>{t('monitoring.audit.duration')}</Table.Th>
                    <Table.Th>{t('monitoring.audit.node')}</Table.Th>
                    <Table.Th>{t('monitoring.audit.client')}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {items.map((e, i) => {
                    const open = expanded.has(i);
                    const resource =
                      e.bucket !== undefined && e.bucket !== ''
                        ? `${e.bucket}${e.object ? `/${e.object}` : ''}`
                        : (e.path ?? '—');
                    return (
                      <Fragment key={`${e.requestId ?? ''}-${String(i)}`}>
                        <Table.Tr>
                          <Table.Td>
                            <ActionIcon
                              variant="subtle"
                              color="gray"
                              aria-expanded={open}
                              aria-label={t('monitoring.audit.toggleDetails', {
                                requestId: e.requestId ?? String(i + 1),
                              })}
                              onClick={() => {
                                setExpanded((cur) => {
                                  const next = new Set(cur);
                                  if (next.has(i)) next.delete(i);
                                  else next.add(i);
                                  return next;
                                });
                              }}
                            >
                              {open ? (
                                <IconChevronDown size={16} />
                              ) : (
                                <IconChevronRight size={16} />
                              )}
                            </ActionIcon>
                          </Table.Td>
                          <Table.Td style={{ whiteSpace: 'nowrap' }}>
                            <time dateTime={e.time}>{new Date(e.time).toLocaleString(locale)}</time>
                          </Table.Td>
                          <Table.Td style={{ overflowWrap: 'anywhere' }}>
                            {e.accessKey ?? '—'}
                            {e.parentUser !== undefined && e.parentUser !== e.accessKey ? (
                              <Text size="xs" c="dimmed">
                                {e.parentUser}
                              </Text>
                            ) : null}
                          </Table.Td>
                          <Table.Td>
                            <Group gap={4} wrap="nowrap">
                              {e.type === 'admin' ? (
                                <Badge size="xs" variant="outline" color="gray">
                                  {t('monitoring.audit.types.admin')}
                                </Badge>
                              ) : null}
                              <Text size="sm">{e.api}</Text>
                            </Group>
                          </Table.Td>
                          <Table.Td style={{ overflowWrap: 'anywhere' }}>
                            <Text size="sm" ff="monospace">
                              {resource}
                            </Text>
                          </Table.Td>
                          <Table.Td>
                            <Badge color={statusColor(e.statusCode)} variant="light">
                              {e.statusCode}
                            </Badge>
                          </Table.Td>
                          <Table.Td style={{ whiteSpace: 'nowrap' }}>
                            {formatMs(e.durationMs, locale)}
                          </Table.Td>
                          <Table.Td>{e.node ?? '—'}</Table.Td>
                          <Table.Td>{e.remoteHost ?? '—'}</Table.Td>
                        </Table.Tr>
                        {open ? (
                          <Table.Tr>
                            <Table.Td colSpan={9} bg="var(--mantine-color-default-hover)">
                              <AuditDetails entry={e} />
                            </Table.Td>
                          </Table.Tr>
                        ) : null}
                      </Fragment>
                    );
                  })}
                </Table.Tbody>
              </Table>
            </ScrollTable>
          )}
        </Paper>
      ) : null}
    </Stack>
  );
}
