import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Modal,
  NumberInput,
  Paper,
  PasswordInput,
  Select,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconPencil, IconPlugConnected, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { distributionKeys } from '@/lib/accessQueries';
import {
  dnsProviderForm,
  dnsProviderInput,
  dnsProviderTarget,
  type DnsProviderForm,
} from '@/lib/dnsProviders';
import {
  useServices,
  type DnsProvider,
  type DnsProviderInput,
  type DnsProviderTestResult,
  type DnsProviderType,
} from '@/services';

const providerTypes: DnsProviderType[] = ['cloudflare', 'route53', 'rfc2136'];
const tsigAlgorithms = ['hmac-sha256', 'hmac-sha512', 'hmac-sha384', 'hmac-sha224', 'hmac-sha1'];

/** The text fields of the form. */
type StringField = Exclude<keyof DnsProviderForm, 'type' | 'propagationTimeout'>;

function ProviderDialog({
  editing,
  opened,
  onClose,
}: {
  /** The provider edited; undefined to add one. */
  editing: DnsProvider | undefined;
  opened: boolean;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const { certificates } = useServices();
  const queryClient = useQueryClient();
  const [form, setForm] = useState<DnsProviderForm>(() => dnsProviderForm(editing));
  const save = useMutation({
    mutationFn: (input: DnsProviderInput) =>
      editing === undefined
        ? certificates.createDnsProvider(input)
        : certificates.updateDnsProvider(editing.name, input),
    onSuccess: async (p) => {
      await queryClient.invalidateQueries({ queryKey: distributionKeys.dnsProviders });
      notifications.show({
        color: 'green',
        message: t('certificates.dns.saved', { name: p.name }),
      });
      onClose();
    },
  });
  const set = (patch: Partial<DnsProviderForm>) => {
    setForm((f) => ({ ...f, ...patch }));
  };
  // The stored secrets are kept when left empty (same type only).
  const stored = editing?.hasCredentials === true && editing.type === form.type;
  const secretHint = (hint?: string) =>
    stored ? t('certificates.dns.keepSecret') : (hint ?? undefined);
  const text = (key: StringField, label: string, description?: string, required = false) => (
    <TextInput
      label={label}
      description={description}
      required={required}
      value={form[key]}
      onChange={(event) => {
        set({ [key]: event.currentTarget.value });
      }}
    />
  );
  const secret = (key: StringField, label: string, hint?: string, required = false) => (
    <PasswordInput
      label={label}
      description={secretHint(hint)}
      required={required && !stored}
      autoComplete="new-password"
      value={form[key]}
      onChange={(event) => {
        set({ [key]: event.currentTarget.value });
      }}
    />
  );
  return (
    <Modal
      opened={opened}
      onClose={onClose}
      size="lg"
      title={
        editing === undefined
          ? t('certificates.dns.addTitle')
          : t('certificates.dns.editTitle', { name: editing.name })
      }
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate(dnsProviderInput(form));
        }}
      >
        <Stack gap="sm">
          <TextInput
            label={t('certificates.dns.name')}
            description={t('certificates.dns.nameHint')}
            required
            disabled={editing !== undefined}
            value={form.name}
            onChange={(event) => {
              set({ name: event.currentTarget.value });
            }}
          />
          <Select
            label={t('certificates.dns.type')}
            data={providerTypes.map((v) => ({ value: v, label: t(`certificates.dns.types.${v}`) }))}
            value={form.type}
            allowDeselect={false}
            onChange={(value) => {
              if (value !== null) set({ type: value as DnsProviderType });
            }}
          />
          {form.type === 'cloudflare' ? (
            <>
              {secret(
                'apiToken',
                t('certificates.dns.apiToken'),
                t('certificates.dns.apiTokenHint'),
                true,
              )}
              {text('zoneId', t('certificates.dns.zoneId'), t('certificates.dns.zoneIdHint'))}
            </>
          ) : null}
          {form.type === 'route53' ? (
            <>
              {text(
                'accessKeyId',
                t('certificates.dns.accessKeyId'),
                t('certificates.dns.accessKeyIdHint'),
              )}
              {form.accessKeyId.trim() !== '' ? (
                <>
                  {secret(
                    'secretAccessKey',
                    t('certificates.dns.secretAccessKey'),
                    undefined,
                    true,
                  )}
                  {secret('sessionToken', t('certificates.dns.sessionToken'))}
                </>
              ) : null}
              {text('region', t('certificates.dns.region'), 'us-east-1')}
              {text(
                'hostedZoneId',
                t('certificates.dns.hostedZoneId'),
                t('certificates.dns.hostedZoneIdHint'),
              )}
            </>
          ) : null}
          {form.type === 'rfc2136' ? (
            <>
              {text(
                'nameserver',
                t('certificates.dns.nameserver'),
                t('certificates.dns.nameserverHint'),
                true,
              )}
              {text('zone', t('certificates.dns.rfcZone'), t('certificates.dns.rfcZoneHint'))}
              {text('tsigKeyName', t('certificates.dns.tsigKeyName'))}
              {form.tsigKeyName.trim() !== '' ? (
                <>
                  <Select
                    label={t('certificates.dns.tsigAlgorithm')}
                    data={tsigAlgorithms}
                    placeholder="hmac-sha256"
                    clearable
                    value={form.tsigAlgorithm === '' ? null : form.tsigAlgorithm}
                    onChange={(value) => {
                      set({ tsigAlgorithm: value ?? '' });
                    }}
                  />
                  {secret('tsigSecret', t('certificates.dns.tsigSecret'), undefined, true)}
                </>
              ) : null}
            </>
          ) : null}
          <NumberInput
            label={t('certificates.dns.propagationTimeout')}
            description={t('certificates.dns.propagationTimeoutHint')}
            min={0}
            max={1800}
            value={form.propagationTimeout}
            onChange={(value) => {
              set({ propagationTimeout: typeof value === 'number' ? value : '' });
            }}
          />
          {text('comment', t('certificates.dns.comment'))}
          {save.isError ? (
            <ErrorAlert error={save.error} title={t('certificates.dns.invalid')} />
          ) : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={save.isPending} disabled={form.name.trim() === ''}>
              {t('certificates.dns.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function TestDialog({ name, onClose }: { name: string | null; onClose: () => void }) {
  const { t } = useTranslation();
  const { certificates } = useServices();
  const [domain, setDomain] = useState('');
  const run = useMutation({
    mutationFn: (input: { name: string; domain: string }) =>
      certificates.testDnsProvider(input.name, input.domain),
  });
  const result: DnsProviderTestResult | undefined = run.data;
  return (
    <Modal
      opened={name !== null}
      onClose={() => {
        run.reset();
        onClose();
      }}
      title={t('certificates.dns.testTitle', { name: name ?? '' })}
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (name !== null) run.mutate({ name, domain: domain.trim() });
        }}
      >
        <Stack gap="sm">
          <Text size="sm" c="dimmed">
            {t('certificates.dns.testHint')}
          </Text>
          <TextInput
            label={t('certificates.dns.testDomain')}
            placeholder="example.com"
            required
            value={domain}
            onChange={(event) => {
              setDomain(event.currentTarget.value);
            }}
          />
          {run.isError ? (
            <ErrorAlert error={run.error} title={t('certificates.dns.invalid')} />
          ) : null}
          {result !== undefined ? (
            <Alert color={result.ok ? 'teal' : 'red'} variant="light" role="status">
              {result.ok
                ? t('certificates.dns.testOk', {
                    record: result.record,
                    seconds: (result.durationMs / 1000).toFixed(1),
                  })
                : t('certificates.dns.testFailed', {
                    step: result.step ?? '',
                    error: result.error ?? '',
                  })}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button type="submit" loading={run.isPending} disabled={domain.trim() === ''}>
              {t('certificates.dns.testRun')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/**
 * The DNS providers of the ACME DNS-01 challenge (the wildcard domains):
 * Cloudflare, Route 53, RFC 2136; their credentials are write-only.
 */
export function DnsProvidersSection({ canManage }: { canManage: boolean }) {
  const { t } = useTranslation();
  const { certificates } = useServices();
  const queryClient = useQueryClient();
  const list = useQuery({
    queryKey: distributionKeys.dnsProviders,
    queryFn: () => certificates.listDnsProviders(),
  });
  const [dialog, setDialog] = useState<{ editing: DnsProvider | undefined } | null>(null);
  const [testing, setTesting] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const remove = useMutation({
    mutationFn: (name: string) => certificates.removeDnsProvider(name),
    onSuccess: async () => {
      setDeleting(null);
      await queryClient.invalidateQueries({ queryKey: distributionKeys.dnsProviders });
    },
  });
  const items = list.data ?? [];
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('certificates.dns.title')}
    >
      <Stack gap="sm">
        <Group justify="space-between">
          <Title order={4}>{t('certificates.dns.title')}</Title>
          {canManage ? (
            <Button
              size="xs"
              leftSection={<IconPlus size={14} />}
              onClick={() => {
                setDialog({ editing: undefined });
              }}
            >
              {t('certificates.dns.add')}
            </Button>
          ) : null}
        </Group>
        <Text size="sm" c="dimmed">
          {t('certificates.dns.hint')}
        </Text>
        {list.isError ? (
          <ErrorAlert error={list.error} title={t('certificates.dns.loadError')} />
        ) : items.length === 0 ? (
          <Text size="sm" c="dimmed">
            {list.isPending ? '…' : t('certificates.dns.empty')}
          </Text>
        ) : (
          <Table.ScrollContainer minWidth={720}>
            <Table aria-label={t('certificates.dns.tableLabel')} verticalSpacing="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('certificates.dns.name')}</Table.Th>
                  <Table.Th>{t('certificates.dns.type')}</Table.Th>
                  <Table.Th>{t('certificates.dns.zone')}</Table.Th>
                  <Table.Th>{t('certificates.dns.credentials')}</Table.Th>
                  <Table.Th>{t('certificates.dns.usedBy')}</Table.Th>
                  <Table.Th w={120} />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {items.map((p) => (
                  <Table.Tr key={p.name}>
                    <Table.Td>
                      <Text size="sm" ff="monospace">
                        {p.name}
                      </Text>
                      {p.comment !== undefined ? (
                        <Text size="xs" c="dimmed">
                          {p.comment}
                        </Text>
                      ) : null}
                    </Table.Td>
                    <Table.Td>{t(`certificates.dns.types.${p.type}`)}</Table.Td>
                    <Table.Td>
                      <Text size="sm">{dnsProviderTarget(p, t('certificates.dns.ambient'))}</Text>
                    </Table.Td>
                    <Table.Td>
                      <Badge variant="light" color={p.hasCredentials ? 'teal' : 'gray'}>
                        {p.hasCredentials
                          ? t('certificates.dns.credentialsStored')
                          : t('certificates.dns.credentialsNone')}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{p.usedBy.length > 0 ? p.usedBy.join(', ') : '—'}</Text>
                    </Table.Td>
                    <Table.Td>
                      {canManage ? (
                        <Group gap={2} wrap="nowrap">
                          <Tooltip label={t('certificates.dns.test', { name: p.name })}>
                            <ActionIcon
                              variant="subtle"
                              aria-label={t('certificates.dns.test', { name: p.name })}
                              onClick={() => {
                                setTesting(p.name);
                              }}
                            >
                              <IconPlugConnected size={16} />
                            </ActionIcon>
                          </Tooltip>
                          <Tooltip label={t('certificates.dns.edit', { name: p.name })}>
                            <ActionIcon
                              variant="subtle"
                              aria-label={t('certificates.dns.edit', { name: p.name })}
                              onClick={() => {
                                setDialog({ editing: p });
                              }}
                            >
                              <IconPencil size={16} />
                            </ActionIcon>
                          </Tooltip>
                          <Tooltip label={t('certificates.dns.delete', { name: p.name })}>
                            <ActionIcon
                              variant="subtle"
                              color="red"
                              aria-label={t('certificates.dns.delete', { name: p.name })}
                              onClick={() => {
                                remove.reset();
                                setDeleting(p.name);
                              }}
                            >
                              <IconTrash size={16} />
                            </ActionIcon>
                          </Tooltip>
                        </Group>
                      ) : null}
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </Stack>
      {dialog !== null ? (
        <ProviderDialog
          key={dialog.editing?.name ?? '+'}
          editing={dialog.editing}
          opened
          onClose={() => {
            setDialog(null);
          }}
        />
      ) : null}
      <TestDialog
        key={testing ?? ''}
        name={testing}
        onClose={() => {
          setTesting(null);
        }}
      />
      <ConfirmDialog
        opened={deleting !== null}
        title={t('certificates.dns.deleteTitle')}
        message={t('certificates.dns.deleteConfirm', { name: deleting ?? '' })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setDeleting(null);
        }}
        onConfirm={() => {
          if (deleting !== null) remove.mutate(deleting);
        }}
      />
    </Paper>
  );
}
