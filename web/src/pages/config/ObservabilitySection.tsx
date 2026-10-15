import {
  Anchor,
  Badge,
  Button,
  Group,
  Modal,
  Paper,
  Select,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconPlus } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { MonitoringActions, usePermissions } from '@/auth/usePermissions';
import { ScrollTable } from '@/components/cache/ScrollTable';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { TelemetryStatus, TlsStatus } from '@/components/observability/ServerTelemetry';
import { checkConfigValue, isConfigTrue } from '@/lib/config';
import { iamKeys } from '@/lib/iamQueries';
import { serverInfoQueryKey } from '@/lib/serverInfo';
import {
  useServices,
  type ConfigSettings,
  type ConfigSubsystem,
  type ConfigSubsystemSchema,
} from '@/services';

import { ConfigField } from './ConfigField';

const loggerLevels = ['debug', 'info', 'warn', 'error'] as const;
const loggerFormats = ['text', 'json'] as const;
const auditTypes = ['webhook', 'kafka'] as const;
type AuditType = (typeof auditTypes)[number];
const idPattern = /^[A-Za-z0-9_.-]{1,64}$/;

/** The configuration subsystem of an audit target ("_" is the default target). */
function auditSubsystem(type: AuditType, id: string): string {
  return id === '' || id === '_' ? `audit_${type}` : `audit_${type}:${id}`;
}

/** The logger subsystem: level and format of the logs, applied at once. */
function LoggerCard() {
  const { t } = useTranslation();
  const { config } = useServices();
  const queryClient = useQueryClient();
  const key = [...iamKeys.config, 'logger'];
  const current = useQuery({ queryKey: key, queryFn: () => config.get('logger') });
  const stored = current.data?.settings ?? {};
  const [level, setLevel] = useState('');
  const [format, setFormat] = useState('');
  useEffect(() => {
    setLevel(current.data?.settings.level ?? '');
    setFormat(current.data?.settings.format ?? '');
  }, [current.data]);
  const save = useMutation({
    mutationFn: async () => {
      const set: ConfigSettings = {};
      const unset: string[] = [];
      for (const [name, value] of [
        ['level', level],
        ['format', format],
      ] as const) {
        if (value === (stored[name] ?? '')) continue;
        if (value === '') unset.push(name);
        else set[name] = value;
      }
      if (Object.keys(set).length > 0) await config.set('logger', set);
      if (unset.length > 0) await config.reset('logger', unset);
    },
    onSuccess: async () => {
      notifications.show({ color: 'teal', message: t('observability.logger.saved') });
      await queryClient.invalidateQueries({ queryKey: iamKeys.config });
    },
  });
  const changed = level !== (stored.level ?? '') || format !== (stored.format ?? '');
  const defaultOption = { value: '', label: t('observability.logger.default') };
  return (
    <Stack gap="xs">
      <Group justify="space-between" wrap="wrap">
        <Title order={4}>{t('observability.logger.title')}</Title>
        <Anchor component={Link} to="/monitoring/logs" size="sm">
          {t('observability.logger.live')}
        </Anchor>
      </Group>
      <Text size="sm" c="dimmed">
        {t('observability.logger.hint')}
      </Text>
      {current.isError ? <ErrorAlert error={current.error} /> : null}
      {save.isError ? <ErrorAlert error={save.error} /> : null}
      <Group align="flex-end" wrap="wrap">
        <Select
          label={t('observability.logger.level')}
          data={[defaultOption, ...loggerLevels.map((v) => ({ value: v, label: v }))]}
          value={level}
          allowDeselect={false}
          disabled={current.isPending}
          onChange={(v) => {
            setLevel(v ?? '');
          }}
          w={200}
        />
        <Select
          label={t('observability.logger.format')}
          data={[defaultOption, ...loggerFormats.map((v) => ({ value: v, label: v }))]}
          value={format}
          allowDeselect={false}
          disabled={current.isPending}
          onChange={(v) => {
            setFormat(v ?? '');
          }}
          w={200}
        />
        <Button
          onClick={() => {
            save.mutate();
          }}
          disabled={!changed}
          loading={save.isPending}
        >
          {t('common.save')}
        </Button>
      </Group>
    </Stack>
  );
}

interface AuditTarget {
  type: AuditType;
  id: string;
}

/** The form of an audit target: the keys of its subsystem. */
function AuditTargetForm({
  target,
  schema,
  onClose,
}: {
  /** The target edited; null for a named target to choose. */
  target: AuditTarget | null;
  schema: readonly ConfigSubsystemSchema[] | undefined;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const { config } = useServices();
  const queryClient = useQueryClient();
  const [type, setType] = useState<AuditType>(target?.type ?? 'webhook');
  const [id, setId] = useState(target?.id ?? '');
  const [draft, setDraft] = useState<ConfigSettings>({});
  const subsystem = auditSubsystem(type, id.trim());
  const idInvalid = id.trim() !== '' && id.trim() !== '_' && !idPattern.test(id.trim());
  const current = useQuery({
    queryKey: [...iamKeys.config, subsystem],
    queryFn: () => config.get(subsystem),
    enabled: !idInvalid,
  });
  useEffect(() => {
    setDraft(current.data?.settings ?? {});
  }, [current.data]);
  const keys = schema?.find((s) => s.name === `audit_${type}`)?.keys ?? [];
  const stored = current.data?.settings ?? {};
  const changed = Object.fromEntries(
    Object.entries(draft).filter(([k, v]) => (stored[k] ?? '') !== v),
  );
  const problems = Object.fromEntries(
    keys.flatMap((key) => {
      const problem = checkConfigValue(key, draft[key.name] ?? '');
      return problem !== undefined ? [[key.name, problem] as const] : [];
    }),
  );
  const save = useMutation({
    mutationFn: () => config.set(subsystem, changed),
    onSuccess: async () => {
      notifications.show({
        color: 'teal',
        message: t('observability.audit.saved', { target: subsystem }),
      });
      await queryClient.invalidateQueries({ queryKey: iamKeys.config });
      onClose();
    },
  });
  return (
    <Modal
      opened
      onClose={onClose}
      title={t('observability.audit.editTitle', { target: subsystem })}
      size="lg"
      centered
    >
      <form
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          if (!idInvalid && Object.keys(problems).length === 0 && Object.keys(changed).length > 0) {
            save.mutate();
          }
        }}
      >
        <Stack gap="sm">
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <Select
              label={t('observability.audit.type')}
              data={auditTypes.map((v) => ({
                value: v,
                label: t(`observability.audit.types.${v}`),
              }))}
              value={type}
              allowDeselect={false}
              disabled={target !== null}
              onChange={(v) => {
                if (v !== null) setType(v as AuditType);
              }}
            />
            <TextInput
              label={t('observability.audit.id')}
              description={t('observability.audit.idHint')}
              placeholder="_"
              value={id}
              disabled={target !== null}
              error={idInvalid ? t('observability.audit.idInvalid') : undefined}
              onChange={(event) => {
                setId(event.currentTarget.value);
              }}
            />
          </SimpleGrid>
          {current.isPending && !idInvalid ? <Skeleton height={120} /> : null}
          {keys.map((key) => (
            <ConfigField
              key={key.name}
              configKey={key}
              value={draft[key.name] ?? ''}
              error={
                problems[key.name] !== undefined
                  ? t(`configuration.problems.${problems[key.name] ?? 'list'}`, {
                      values: (key.values ?? []).join(', '),
                    })
                  : undefined
              }
              onChange={(value) => {
                setDraft((d) => ({ ...d, [key.name]: value }));
              }}
            />
          ))}
          {save.isError ? <ErrorAlert error={save.error} /> : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button
              type="submit"
              loading={save.isPending}
              disabled={idInvalid || Object.keys(changed).length === 0}
            >
              {t('common.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/** Where a target delivers: the URL of a webhook, the brokers of Kafka. */
function destinationOf(type: AuditType, settings: ConfigSettings | undefined): string {
  if (settings === undefined) return '';
  return (type === 'webhook' ? settings.endpoint : settings.brokers) ?? '';
}

/** The audit targets (audit_webhook, audit_kafka and the named ones). */
function AuditTargetsCard({
  schema,
  values,
}: {
  schema: readonly ConfigSubsystemSchema[] | undefined;
  values: readonly ConfigSubsystem[] | undefined;
}) {
  const { t } = useTranslation();
  const { config } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const [editing, setEditing] = useState<AuditTarget | null | undefined>(undefined);
  const [removing, setRemoving] = useState<AuditTarget | null>(null);
  const remove = useMutation({
    mutationFn: (target: AuditTarget) => config.reset(auditSubsystem(target.type, target.id)),
    onSuccess: async () => {
      setRemoving(null);
      await queryClient.invalidateQueries({ queryKey: iamKeys.config });
    },
  });
  return (
    <Stack gap="xs">
      <Group justify="space-between" wrap="wrap">
        <Title order={4}>{t('observability.audit.title')}</Title>
        <Group gap="xs">
          {perms.can(MonitoringActions.consoleLog) ? (
            <Anchor component={Link} to="/monitoring/audit" size="sm">
              {t('observability.audit.search')}
            </Anchor>
          ) : null}
          <Button
            size="xs"
            variant="default"
            leftSection={<IconPlus size={14} />}
            onClick={() => {
              setEditing(null);
            }}
          >
            {t('observability.audit.addNamed')}
          </Button>
        </Group>
      </Group>
      <Text size="sm" c="dimmed">
        {t('observability.audit.hint')}
      </Text>
      {remove.isError ? <ErrorAlert error={remove.error} /> : null}
      <ScrollTable minWidth={560} label={t('observability.audit.title')}>
        <Table aria-label={t('observability.audit.title')} verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('observability.audit.target')}</Table.Th>
              <Table.Th>{t('observability.audit.destination')}</Table.Th>
              <Table.Th>{t('observability.audit.state')}</Table.Th>
              <Table.Th>{t('common.actions')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {auditTypes.map((type) => {
              const name = auditSubsystem(type, '');
              const settings = values?.find((v) => v.name === name)?.settings;
              const configured = settings !== undefined && Object.keys(settings).length > 0;
              const enabled = configured && isConfigTrue(settings.enable);
              return (
                <Table.Tr key={type}>
                  <Table.Td>
                    <Stack gap={0}>
                      <Text size="sm">{t(`observability.audit.types.${type}`)}</Text>
                      <Text size="xs" c="dimmed" ff="monospace">
                        {name}
                      </Text>
                    </Stack>
                  </Table.Td>
                  <Table.Td style={{ overflowWrap: 'anywhere' }}>
                    <Text size="sm" ff="monospace">
                      {destinationOf(type, settings) || '—'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge
                      variant="light"
                      color={!configured ? 'gray' : enabled ? 'teal' : 'yellow'}
                    >
                      {!configured
                        ? t('observability.audit.notConfigured')
                        : enabled
                          ? t('observability.audit.enabled')
                          : t('observability.audit.disabled')}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Group gap="xs" wrap="nowrap">
                      <Button
                        size="xs"
                        variant="default"
                        aria-label={t('observability.audit.edit', { target: name })}
                        onClick={() => {
                          setEditing({ type, id: '' });
                        }}
                      >
                        {t('observability.audit.configure')}
                      </Button>
                      {configured ? (
                        <Button
                          size="xs"
                          variant="subtle"
                          color="red"
                          aria-label={t('observability.audit.remove', { target: name })}
                          onClick={() => {
                            remove.reset();
                            setRemoving({ type, id: '' });
                          }}
                        >
                          {t('common.delete')}
                        </Button>
                      ) : null}
                    </Group>
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      </ScrollTable>
      {editing !== undefined ? (
        <AuditTargetForm
          target={editing}
          schema={schema}
          onClose={() => {
            setEditing(undefined);
          }}
        />
      ) : null}
      <ConfirmDialog
        opened={removing !== null}
        title={t('observability.audit.removeTitle')}
        message={t('observability.audit.removeConfirm', {
          target: removing ? auditSubsystem(removing.type, removing.id) : '',
        })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setRemoving(null);
        }}
        onConfirm={() => {
          if (removing) remove.mutate(removing);
        }}
      />
    </Stack>
  );
}

/**
 * D-CFG-01, observability: the level and the format of the logs (logger),
 * the audit targets (audit_webhook, audit_kafka), and the state of the
 * distributed tracing and of the TLS certificates of the server (from GET
 * /info, set by the environment and the certificates directory).
 */
export function ObservabilitySection({
  schema,
  values,
}: {
  schema: readonly ConfigSubsystemSchema[] | undefined;
  values: readonly ConfigSubsystem[] | undefined;
}) {
  const { t } = useTranslation();
  const { info } = useServices();
  const perms = usePermissions();
  const canInfo = perms.can(MonitoringActions.serverInfo);
  const server = useQuery({
    queryKey: serverInfoQueryKey,
    queryFn: () => info.serverInfo(),
    enabled: canInfo,
  });
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      id="observability"
      aria-label={t('observability.title')}
    >
      <Stack gap="lg">
        <Stack gap={2}>
          <Title order={3}>{t('observability.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('observability.subtitle')}
          </Text>
        </Stack>
        <LoggerCard />
        <AuditTargetsCard schema={schema} values={values} />
        <Stack gap="xs">
          <Title order={4}>{t('observability.telemetry.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('observability.telemetry.hint')}
          </Text>
          {!canInfo ? (
            <Text size="sm" c="dimmed">
              {t('observability.infoUnavailable')}
            </Text>
          ) : server.isPending ? (
            <Skeleton height={80} />
          ) : server.isError ? (
            <ErrorAlert error={server.error} />
          ) : (
            <TelemetryStatus telemetry={server.data.telemetry} />
          )}
        </Stack>
        <Stack gap="xs">
          <Title order={4}>{t('observability.tls.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('observability.tls.hint')}
          </Text>
          {!canInfo ? (
            <Text size="sm" c="dimmed">
              {t('observability.infoUnavailable')}
            </Text>
          ) : server.isPending ? (
            <Skeleton height={80} />
          ) : server.isError ? (
            <ErrorAlert error={server.error} />
          ) : (
            <TlsStatus tls={server.data.tls} />
          )}
        </Stack>
      </Stack>
    </Paper>
  );
}
