import {
  ActionIcon,
  Badge,
  Button,
  Group,
  Modal,
  Paper,
  Select,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconEdit, IconPlus, IconSend, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { checkConfigValue } from '@/lib/config';
import { targetRef } from '@/lib/events';
import { iamKeys } from '@/lib/iamQueries';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import {
  useServices,
  type ConfigSettings,
  type ConfigSubsystemSchema,
  type NotifyTarget,
  type NotifyTargetType,
} from '@/services';

import { ConfigField } from './ConfigField';

const targetTypes: readonly NotifyTargetType[] = [
  'webhook',
  'kafka',
  'nats',
  'amqp',
  'redis',
  'postgresql',
];

/** The configuration subsystem of a target ("_" is the default target of its type). */
function subsystemOf(type: NotifyTargetType, id: string): string {
  const base = type === 'postgresql' ? 'notify_postgres' : `notify_${type}`;
  return id === '' || id === '_' ? base : `${base}:${id}`;
}

/** The schema of the subsystem of a type. */
function schemaOf(
  schema: readonly ConfigSubsystemSchema[] | undefined,
  type: NotifyTargetType,
): ConfigSubsystemSchema | undefined {
  return schema?.find((s) => s.name === subsystemOf(type, '_'));
}

const idPattern = /^[A-Za-z0-9_.-]{1,64}$/;

interface TargetFormProps {
  opened: boolean;
  /** The target edited; null for a new one. */
  target: NotifyTarget | null;
  schema: readonly ConfigSubsystemSchema[] | undefined;
  onClose: () => void;
}

/** Adds or edits a target: its type, its id and the keys of its subsystem. */
function TargetForm({ opened, target, schema, onClose }: TargetFormProps) {
  const { t } = useTranslation();
  const { config } = useServices();
  const queryClient = useQueryClient();
  const [type, setType] = useState<NotifyTargetType>('webhook');
  const [id, setId] = useState('');
  const [draft, setDraft] = useState<ConfigSettings>({});
  const subsystem = subsystemOf(type, id.trim());
  const current = useQuery({
    queryKey: [...iamKeys.config, subsystem],
    queryFn: () => config.get(subsystem),
    enabled: opened && target !== null,
  });
  useEffect(() => {
    if (!opened) return;
    setType(target?.type ?? 'webhook');
    setId(target === null ? '' : target.id);
    setDraft(target === null ? { enable: 'on' } : {});
  }, [opened, target]);
  useEffect(() => {
    if (current.data !== undefined) setDraft(current.data.settings);
  }, [current.data]);

  const keys = schemaOf(schema, type)?.keys ?? [];
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
  const idInvalid = id.trim() !== '' && id.trim() !== '_' && !idPattern.test(id.trim());
  const save = useMutation({
    mutationFn: () => config.set(subsystem, changed),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.notifyTargets });
      await queryClient.invalidateQueries({ queryKey: iamKeys.config });
      notifications.show({
        color: 'green',
        message: t('notifyTargets.saved', { target: subsystem }),
      });
      onClose();
    },
  });

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={
        target === null
          ? t('notifyTargets.addTitle')
          : t('notifyTargets.editTitle', { target: targetRef(target) })
      }
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
          <Group grow align="flex-start" wrap="wrap">
            <Select
              label={t('notifyTargets.type')}
              data={targetTypes.map((value) => ({
                value,
                label: t(`notifyTargets.types.${value}`),
              }))}
              value={type}
              allowDeselect={false}
              disabled={target !== null}
              onChange={(next) => {
                if (next !== null) {
                  setType(next as NotifyTargetType);
                  setDraft({ enable: 'on' });
                }
              }}
            />
            <TextInput
              label={t('notifyTargets.id')}
              description={t('notifyTargets.idHint')}
              placeholder="_"
              value={id}
              disabled={target !== null}
              error={idInvalid ? t('notifyTargets.idInvalid') : undefined}
              onChange={(event) => {
                setId(event.currentTarget.value);
              }}
            />
          </Group>
          <Text size="xs" c="dimmed">
            {t('notifyTargets.subsystem', { subsystem })}
          </Text>
          {target !== null && current.isPending ? <Skeleton height={120} /> : null}
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
              {t('notifyTargets.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/**
 * D-CFG-01: the notification targets (notify_* subsystems) with their
 * delivery state, a form per type, the test event and the removal.
 */
export function NotifyTargetsSection({
  schema,
}: {
  schema: readonly ConfigSubsystemSchema[] | undefined;
}) {
  const { t } = useTranslation();
  const { notifications: service, config } = useServices();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<NotifyTarget | null | undefined>(undefined);
  const [removing, setRemoving] = useState<NotifyTarget | null>(null);
  const [results, setResults] = useState<Record<string, { ok: boolean; text: string }>>({});
  const targets = useQuery({
    queryKey: storageKeys.notifyTargets,
    queryFn: () => service.targets(),
  });
  const test = useMutation({
    mutationFn: (target: NotifyTarget) => service.test(targetRef(target)),
    onSuccess: async (result, target) => {
      const ref = targetRef(target);
      const text = result.delivered
        ? t('bucketEvents.testDelivered', { target: ref })
        : t('bucketEvents.testFailed', { target: ref, error: result.error ?? '' });
      setResults((r) => ({ ...r, [ref]: { ok: result.delivered, text } }));
      await queryClient.invalidateQueries({ queryKey: storageKeys.notifyTargets });
    },
    onError: (error, target) => {
      setResults((r) => ({
        ...r,
        [targetRef(target)]: { ok: false, text: problemMessage(t, error) },
      }));
    },
  });
  const remove = useMutation({
    mutationFn: (target: NotifyTarget) => config.reset(subsystemOf(target.type, target.id)),
    onSuccess: async () => {
      setRemoving(null);
      await queryClient.invalidateQueries({ queryKey: storageKeys.notifyTargets });
      await queryClient.invalidateQueries({ queryKey: iamKeys.config });
    },
  });

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      id="notify"
      aria-label={t('notifyTargets.title')}
    >
      <Stack gap="sm">
        <Group justify="space-between" align="flex-start" wrap="wrap">
          <Stack gap={2}>
            <Title order={3}>{t('notifyTargets.title')}</Title>
            <Text size="sm" c="dimmed">
              {t('notifyTargets.description')}
            </Text>
          </Stack>
          <Button
            leftSection={<IconPlus size={16} />}
            onClick={() => {
              setEditing(null);
            }}
          >
            {t('notifyTargets.add')}
          </Button>
        </Group>
        {targets.isPending ? (
          <Skeleton height={60} />
        ) : targets.isError ? (
          <ErrorAlert error={targets.error} />
        ) : targets.data.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('notifyTargets.empty')}
          </Text>
        ) : (
          <Table.ScrollContainer minWidth={640}>
            <Table aria-label={t('notifyTargets.title')} verticalSpacing="sm">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('bucketEvents.columns.target')}</Table.Th>
                  <Table.Th>{t('notifyTargets.arn')}</Table.Th>
                  <Table.Th>{t('bucketEvents.columns.state')}</Table.Th>
                  <Table.Th>{t('bucketEvents.columns.pending')}</Table.Th>
                  <Table.Th>{t('common.actions')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {targets.data.map((target) => {
                  const ref = targetRef(target);
                  const result = results[ref];
                  return (
                    <Table.Tr key={ref}>
                      <Table.Td>
                        <Stack gap={0}>
                          <Text size="sm" ff="monospace">
                            {ref}
                          </Text>
                          {target.endpoint !== undefined && target.endpoint !== '' ? (
                            <Text size="xs" c="dimmed" style={{ overflowWrap: 'anywhere' }}>
                              {target.endpoint}
                            </Text>
                          ) : null}
                        </Stack>
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                          {target.arn}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Stack gap={2}>
                          <Badge
                            variant="light"
                            color={!target.enabled ? 'gray' : target.online ? 'green' : 'red'}
                          >
                            {!target.enabled
                              ? t('bucketEvents.state.disabled')
                              : target.online
                                ? t('bucketEvents.state.online')
                                : t('bucketEvents.state.offline')}
                          </Badge>
                          {target.lastError !== undefined && target.lastError !== '' ? (
                            <Text size="xs" c="red" style={{ overflowWrap: 'anywhere' }}>
                              {target.lastError}
                            </Text>
                          ) : null}
                        </Stack>
                      </Table.Td>
                      <Table.Td>{target.pendingEvents}</Table.Td>
                      <Table.Td>
                        <Stack gap={4}>
                          <Group gap={4} wrap="nowrap">
                            <Tooltip label={t('bucketEvents.sendTestTo', { target: ref })}>
                              <ActionIcon
                                variant="subtle"
                                aria-label={t('bucketEvents.sendTestTo', { target: ref })}
                                disabled={!target.enabled}
                                loading={test.isPending && test.variables === target}
                                onClick={() => {
                                  test.mutate(target);
                                }}
                              >
                                <IconSend size={16} />
                              </ActionIcon>
                            </Tooltip>
                            <Tooltip label={t('notifyTargets.edit', { target: ref })}>
                              <ActionIcon
                                variant="subtle"
                                aria-label={t('notifyTargets.edit', { target: ref })}
                                onClick={() => {
                                  setEditing(target);
                                }}
                              >
                                <IconEdit size={16} />
                              </ActionIcon>
                            </Tooltip>
                            <Tooltip label={t('notifyTargets.remove', { target: ref })}>
                              <ActionIcon
                                variant="subtle"
                                color="red"
                                aria-label={t('notifyTargets.remove', { target: ref })}
                                onClick={() => {
                                  remove.reset();
                                  setRemoving(target);
                                }}
                              >
                                <IconTrash size={16} />
                              </ActionIcon>
                            </Tooltip>
                          </Group>
                          {result !== undefined ? (
                            <Text size="xs" c={result.ok ? 'green' : 'red'} role="status">
                              {result.text}
                            </Text>
                          ) : null}
                        </Stack>
                      </Table.Td>
                    </Table.Tr>
                  );
                })}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </Stack>
      <TargetForm
        opened={editing !== undefined}
        target={editing ?? null}
        schema={schema}
        onClose={() => {
          setEditing(undefined);
        }}
      />
      <ConfirmDialog
        opened={removing !== null}
        title={t('notifyTargets.removeTitle')}
        message={t('notifyTargets.removeConfirm', {
          target: removing === null ? '' : targetRef(removing),
        })}
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
