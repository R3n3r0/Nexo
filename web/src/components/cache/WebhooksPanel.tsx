import {
  Alert,
  Badge,
  Button,
  Group,
  Modal,
  MultiSelect,
  Paper,
  PasswordInput,
  Select,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconPlugConnected, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { KeyValueEditor } from '@/components/common/KeyValueEditor';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { cacheKeys, isValidPolicyName } from '@/lib/cache';
import { problemMessage } from '@/lib/problem';
import {
  useServices,
  type CacheWebhook,
  type CacheWebhookTestResult,
  type CacheWebhookType,
} from '@/services';
import { ScrollTable } from './ScrollTable';

const webhookTypes: readonly CacheWebhookType[] = [
  'cloudflare',
  'fastly',
  'akamai',
  'nginx',
  'varnish',
  'generic',
];

type WebhookField = 'url' | 'zone' | 'token' | 'clientToken' | 'clientSecret';

/** The fields a type of webhook needs (internal/service/cache validateWebhook). */
function requiredFields(type: CacheWebhookType): readonly WebhookField[] {
  switch (type) {
    case 'cloudflare':
    case 'fastly':
      return ['zone', 'token'];
    case 'akamai':
      return ['url', 'token', 'clientToken', 'clientSecret'];
    default:
      return ['url'];
  }
}

/** The required fields still empty. */
function missingFields(w: CacheWebhook): WebhookField[] {
  return requiredFields(w.type).filter((f) => (w[f] ?? '').trim() === '');
}

/** The webhook without its empty optional fields. */
function compact(w: CacheWebhook): CacheWebhook {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(w)) {
    if (v === undefined || v === '') continue;
    if (Array.isArray(v) && v.length === 0 && k !== 'domains') continue;
    if (typeof v === 'object' && !Array.isArray(v) && Object.keys(v).length === 0) continue;
    out[k] = v;
  }
  return out as unknown as CacheWebhook;
}

function AddWebhookModal({
  opened,
  domains,
  onClose,
  onAdd,
  pending,
  error,
}: {
  opened: boolean;
  domains: readonly string[];
  onClose: () => void;
  onAdd: (w: CacheWebhook) => void;
  pending: boolean;
  error: unknown;
}) {
  const { t } = useTranslation();
  const [w, setW] = useState<CacheWebhook>({ id: '', type: 'cloudflare', domains: [] });
  const set = (patch: Partial<CacheWebhook>) => {
    setW({ ...w, ...patch });
  };
  const idInvalid = w.id !== '' && !isValidPolicyName(w.id);
  const missing = missingFields(w);
  const field = (name: 'url' | 'zone' | 'clientToken' | 'cpCode' | 'method', hint?: string) => (
    <TextInput
      label={t(`cache.webhooks.fields.${name}`)}
      description={hint}
      value={w[name] ?? ''}
      required={(requiredFields(w.type) as readonly string[]).includes(name)}
      onChange={(event) => {
        set({ [name]: event.currentTarget.value });
      }}
    />
  );
  const secret = (name: 'token' | 'clientSecret') => (
    <PasswordInput
      label={t(`cache.webhooks.fields.${name}`)}
      description={t('cache.webhooks.secretHint')}
      value={w[name] ?? ''}
      required
      onChange={(event) => {
        set({ [name]: event.currentTarget.value });
      }}
    />
  );
  return (
    <Modal opened={opened} onClose={onClose} title={t('cache.webhooks.addTitle')} size="lg">
      <Stack
        gap="sm"
        component="form"
        onSubmit={(event) => {
          event.preventDefault();
          onAdd(compact(w));
        }}
      >
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <TextInput
            label={t('cache.webhooks.fields.id')}
            description={t('cache.webhooks.idHint')}
            value={w.id}
            required
            error={idInvalid ? t('cache.webhooks.invalidId') : undefined}
            onChange={(event) => {
              set({ id: event.currentTarget.value.trim() });
            }}
          />
          <Select
            label={t('cache.webhooks.fields.type')}
            allowDeselect={false}
            data={webhookTypes.map((type) => ({
              value: type,
              label: t(`cache.webhooks.types.${type}`),
            }))}
            value={w.type}
            onChange={(type) => {
              if (type !== null) set({ type: type as CacheWebhookType });
            }}
          />
        </SimpleGrid>
        <MultiSelect
          label={t('cache.webhooks.fields.domains')}
          description={t('cache.webhooks.domainsHint')}
          data={[...domains]}
          value={w.domains ?? []}
          searchable
          onChange={(value) => {
            set({ domains: value });
          }}
        />
        {w.type === 'cloudflare' || w.type === 'fastly' ? (
          <>
            {field('zone', t(`cache.webhooks.zoneHint.${w.type}`))}
            {secret('token')}
            {field('url', t('cache.webhooks.apiHint'))}
          </>
        ) : w.type === 'akamai' ? (
          <>
            {field('url', t('cache.webhooks.akamaiHostHint'))}
            {secret('token')}
            {field('clientToken')}
            {secret('clientSecret')}
            <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
              <Select
                label={t('cache.webhooks.fields.network')}
                clearable
                data={['production', 'staging']}
                value={w.network ?? null}
                onChange={(network) => {
                  set({ network: (network ?? undefined) as CacheWebhook['network'] });
                }}
              />
              {field('cpCode', t('cache.webhooks.cpCodeHint'))}
            </SimpleGrid>
          </>
        ) : w.type === 'generic' ? (
          <>
            {field('url', t('cache.webhooks.templateHint'))}
            {field('method')}
            <KeyValueEditor
              label={t('cache.webhooks.fields.headers')}
              value={w.headers ?? {}}
              keyLabel={t('cache.webhooks.headerName')}
              valueLabel={t('cache.webhooks.headerValue')}
              onChange={(headers) => {
                set({ headers });
              }}
            />
            <Textarea
              label={t('cache.webhooks.fields.body')}
              description={t('cache.webhooks.bodyHint')}
              autosize
              minRows={3}
              ff="monospace"
              value={w.body ?? ''}
              onChange={(event) => {
                set({ body: event.currentTarget.value });
              }}
            />
          </>
        ) : (
          field('url', t(`cache.webhooks.proxyHint.${w.type}`))
        )}
        {error !== null && error !== undefined ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, error)}
          </Alert>
        ) : null}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button
            type="submit"
            loading={pending}
            disabled={w.id === '' || idInvalid || missing.length > 0}
          >
            {t('cache.webhooks.add')}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

function TestResult({ result }: { result: CacheWebhookTestResult }) {
  const { t } = useTranslation();
  return result.ok ? (
    <Badge color="teal" variant="light" data-ok="true">
      {t('cache.webhooks.testOk', {
        status: result.status ?? '—',
        ms: result.durationMs,
      })}
    </Badge>
  ) : (
    <Text size="xs" c="red" data-ok="false">
      {t('cache.webhooks.testFailed', {
        error: result.error ?? String(result.status ?? ''),
        ms: result.durationMs,
      })}
    </Text>
  );
}

export interface WebhooksPanelProps {
  distribution: string;
  /** The domains of the distribution (its aliases and overridden hosts). */
  domains: readonly string[];
  /** Changes are refused while the cache settings have unsaved edits. */
  locked?: boolean;
  /** The settings changed on the server (a webhook added or removed). */
  onChanged?: () => void;
}

/**
 * D-CCH-07: the purge webhooks of a distribution (CDN and proxies in
 * front of Nexo, called on every invalidation of their domains, manual
 * or automatic, with retries), with a connection test.
 */
export function WebhooksPanel({
  distribution,
  domains,
  locked = false,
  onChanged,
}: WebhooksPanelProps) {
  const { t } = useTranslation();
  const { cache } = useServices();
  const queryClient = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<string | null>(null);
  const [results, setResults] = useState<Record<string, CacheWebhookTestResult>>({});
  const key = cacheKeys.webhooks(distribution);
  const list = useQuery({ queryKey: key, queryFn: () => cache.webhooks(distribution) });
  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: key });
    onChanged?.();
  };
  const add = useMutation({
    mutationFn: (w: CacheWebhook) => cache.addWebhook(distribution, w),
    onSuccess: async (w) => {
      setAdding(false);
      notifications.show({ color: 'green', message: t('cache.webhooks.added', { id: w.id }) });
      await refresh();
    },
  });
  const remove = useMutation({
    mutationFn: (id: string) => cache.removeWebhook(distribution, id),
    onSuccess: async () => {
      setRemoving(null);
      await refresh();
    },
  });
  const test = useMutation({
    mutationFn: (id: string) => cache.testWebhook(distribution, id),
    onSuccess: (result, id) => {
      setResults((r) => ({ ...r, [id]: result }));
    },
  });
  const title = t('cache.webhooks.title');
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Title order={4}>{title}</Title>
          <Button
            size="xs"
            variant="light"
            leftSection={<IconPlus size={14} />}
            disabled={locked}
            onClick={() => {
              add.reset();
              setAdding(true);
            }}
          >
            {t('cache.webhooks.add')}
          </Button>
        </Group>
        <Text size="sm" c="dimmed">
          {locked ? t('cache.webhooks.locked') : t('cache.webhooks.hint')}
        </Text>
        {list.isPending ? (
          <Skeleton height={48} />
        ) : list.isError ? (
          <ErrorAlert error={list.error} title={t('cache.webhooks.loadError')} />
        ) : list.data.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('cache.webhooks.none')}
          </Text>
        ) : (
          <ScrollTable minWidth={640} label={title}>
            <Table aria-label={title} verticalSpacing={4}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('cache.webhooks.fields.id')}</Table.Th>
                  <Table.Th>{t('cache.webhooks.fields.type')}</Table.Th>
                  <Table.Th>{t('cache.webhooks.fields.domains')}</Table.Th>
                  <Table.Th>{t('cache.webhooks.target')}</Table.Th>
                  <Table.Th>{t('common.actions')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {list.data.map((w) => (
                  <Table.Tr key={w.id}>
                    <Table.Td>
                      <Text size="sm" ff="monospace">
                        {w.id}
                      </Text>
                    </Table.Td>
                    <Table.Td>{t(`cache.webhooks.types.${w.type}`)}</Table.Td>
                    <Table.Td>
                      <Text size="xs" ff="monospace">
                        {(w.domains ?? []).length === 0
                          ? t('cache.webhooks.everyDomain')
                          : (w.domains ?? []).join(', ')}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" ff="monospace" style={{ wordBreak: 'break-all' }}>
                        {w.url ?? w.zone ?? '—'}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Stack gap={4}>
                        <Group gap={4} wrap="nowrap">
                          <Button
                            size="compact-xs"
                            variant="default"
                            leftSection={<IconPlugConnected size={12} />}
                            loading={test.isPending && test.variables === w.id}
                            aria-label={t('cache.webhooks.testOf', { id: w.id })}
                            onClick={() => {
                              test.mutate(w.id);
                            }}
                          >
                            {t('cache.webhooks.test')}
                          </Button>
                          <Button
                            size="compact-xs"
                            variant="subtle"
                            color="red"
                            leftSection={<IconTrash size={12} />}
                            disabled={locked}
                            aria-label={t('cache.webhooks.removeOf', { id: w.id })}
                            onClick={() => {
                              remove.reset();
                              setRemoving(w.id);
                            }}
                          >
                            {t('common.delete')}
                          </Button>
                        </Group>
                        <div aria-live="polite">
                          {results[w.id] !== undefined ? (
                            <TestResult result={results[w.id] as CacheWebhookTestResult} />
                          ) : null}
                        </div>
                      </Stack>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </ScrollTable>
        )}
        {test.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, test.error)}
          </Alert>
        ) : null}
      </Stack>
      {adding ? (
        <AddWebhookModal
          opened={adding}
          domains={domains}
          pending={add.isPending}
          error={add.isError ? add.error : null}
          onClose={() => {
            setAdding(false);
          }}
          onAdd={(w) => {
            add.mutate(w);
          }}
        />
      ) : null}
      <ConfirmDialog
        opened={removing !== null}
        title={t('cache.webhooks.removeTitle')}
        message={t('cache.webhooks.removeConfirm', { id: removing ?? '' })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setRemoving(null);
        }}
        onConfirm={() => {
          if (removing !== null) remove.mutate(removing);
        }}
      />
    </Paper>
  );
}
