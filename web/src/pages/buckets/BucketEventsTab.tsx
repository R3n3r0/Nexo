import {
  ActionIcon,
  Alert,
  Anchor,
  Badge,
  Button,
  Group,
  Modal,
  MultiSelect,
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
import { Link, useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import {
  eventGroups,
  normalizeEvents,
  parseTargetArn,
  targetRef,
  validateSubscription,
  type SubscriptionIssue,
} from '@/lib/events';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import {
  isApiError,
  useServices,
  type EventSubscription,
  type NotifyTarget,
  type RuleProblem,
} from '@/services';

function ruleProblemsOf(error: unknown): readonly RuleProblem[] {
  return isApiError(error) ? error.ruleErrors : [];
}

/** Online/offline badge of a target. */
function TargetState({ target }: { target: NotifyTarget }) {
  const { t } = useTranslation();
  if (!target.enabled) {
    return (
      <Badge variant="light" color="gray">
        {t('bucketEvents.state.disabled')}
      </Badge>
    );
  }
  return (
    <Badge variant="light" color={target.online ? 'green' : 'red'}>
      {target.online ? t('bucketEvents.state.online') : t('bucketEvents.state.offline')}
    </Badge>
  );
}

/** The name of the target of an ARN, with its type. */
function targetLabel(arn: string, targets: readonly NotifyTarget[] | undefined): string {
  const target = targets?.find((t) => t.arn === arn);
  if (target) return `${target.id}:${target.type}`;
  const parsed = parseTargetArn(arn);
  return parsed ? `${parsed.id}:${parsed.type}` : arn;
}

interface SubscriptionEditorProps {
  opened: boolean;
  subscription: EventSubscription | null;
  targets: readonly NotifyTarget[] | undefined;
  saving: boolean;
  problems: readonly RuleProblem[];
  error: string | undefined;
  onSave: (subscription: EventSubscription) => void;
  onClose: () => void;
}

/** Editor of one subscription: target, events, prefix and suffix filters. */
function SubscriptionEditor({
  opened,
  subscription,
  targets,
  saving,
  problems,
  error,
  onSave,
  onClose,
}: SubscriptionEditorProps) {
  const { t } = useTranslation();
  const [id, setId] = useState('');
  const [arn, setArn] = useState('');
  const [events, setEvents] = useState<string[]>([]);
  const [prefix, setPrefix] = useState('');
  const [suffix, setSuffix] = useState('');
  const [submitted, setSubmitted] = useState(false);

  useEffect(() => {
    if (!opened) return;
    setId(subscription?.id ?? '');
    setArn(subscription?.arn ?? '');
    setEvents(subscription?.events ?? ['s3:ObjectCreated:*']);
    setPrefix(subscription?.prefix ?? '');
    setSuffix(subscription?.suffix ?? '');
    setSubmitted(false);
  }, [opened, subscription]);
  // A new subscription goes to the first target by default.
  const firstTarget = targets?.[0]?.arn;
  useEffect(() => {
    if (opened && subscription === null && firstTarget !== undefined) {
      setArn((current) => (current === '' ? firstTarget : current));
    }
  }, [opened, subscription, firstTarget]);

  const draft: EventSubscription = {
    ...(id.trim() !== '' ? { id: id.trim() } : {}),
    arn: arn.trim(),
    events: normalizeEvents(events),
    ...(prefix !== '' ? { prefix } : {}),
    ...(suffix !== '' ? { suffix } : {}),
  };
  const issues: SubscriptionIssue[] = validateSubscription(draft, targets);
  const issue = (kind: readonly SubscriptionIssue[]) => {
    const found = submitted ? issues.find((i) => kind.includes(i)) : undefined;
    return found === undefined ? undefined : t(`bucketEvents.issues.${found}`);
  };

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={subscription === null ? t('bucketEvents.addTitle') : t('bucketEvents.editTitle')}
      size="lg"
      centered
    >
      <form
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          setSubmitted(true);
          if (issues.length === 0) onSave(draft);
        }}
      >
        <Stack gap="md">
          {targets !== undefined ? (
            <Select
              label={t('bucketEvents.target')}
              description={t('bucketEvents.targetHint')}
              placeholder={t('bucketEvents.targetPlaceholder')}
              data={targets.map((target) => ({
                value: target.arn,
                label: `${targetRef(target)}${target.endpoint ? ` — ${target.endpoint}` : ''}`,
              }))}
              value={arn === '' ? null : arn}
              allowDeselect={false}
              nothingFoundMessage={t('bucketEvents.noTargets')}
              error={issue(['arn', 'unknownTarget'])}
              onChange={(next) => {
                setArn(next ?? '');
              }}
            />
          ) : (
            <TextInput
              label={t('bucketEvents.targetArn')}
              description={t('bucketEvents.targetArnHint')}
              placeholder="arn:nexo:sqs:us-east-1:_:webhook"
              value={arn}
              error={issue(['arn', 'unknownTarget'])}
              onChange={(event) => {
                setArn(event.currentTarget.value);
              }}
            />
          )}
          <MultiSelect
            label={t('bucketEvents.events')}
            description={t('bucketEvents.eventsHint')}
            data={eventGroups.map((group) => ({
              group: group.family.replace(':*', ''),
              items: [
                { value: group.family, label: t('bucketEvents.allOf', { family: group.family }) },
                ...group.events.map((e) => ({ value: e, label: e })),
              ],
            }))}
            value={events}
            searchable
            error={issue(['events', 'unknownEvent'])}
            onChange={setEvents}
          />
          <Group grow align="flex-start" wrap="wrap">
            <TextInput
              label={t('bucketEvents.prefix')}
              placeholder="images/"
              value={prefix}
              onChange={(event) => {
                setPrefix(event.currentTarget.value);
              }}
            />
            <TextInput
              label={t('bucketEvents.suffix')}
              placeholder=".jpg"
              value={suffix}
              onChange={(event) => {
                setSuffix(event.currentTarget.value);
              }}
            />
          </Group>
          <TextInput
            label={t('bucketEvents.id')}
            description={t('bucketEvents.idHint')}
            value={id}
            error={issue(['idTooLong'])}
            onChange={(event) => {
              setId(event.currentTarget.value);
            }}
          />
          {problems.length > 0 || error !== undefined ? (
            <Alert color="red" variant="light" role="alert" title={t('bucketEvents.serverRefused')}>
              <Stack gap={2}>
                {problems.map((p) => (
                  <Text key={`${p.code}-${p.message}`} size="sm">
                    {p.message} ({p.code})
                  </Text>
                ))}
                {error !== undefined ? <Text size="sm">{error}</Text> : null}
              </Stack>
            </Alert>
          ) : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={saving}>
              {t('bucketEvents.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/** State of the targets and the test event (admin:ConfigUpdate). */
function TargetsSection({ targets }: { targets: readonly NotifyTarget[] }) {
  const { t } = useTranslation();
  const { notifications: service } = useServices();
  const queryClient = useQueryClient();
  const [results, setResults] = useState<Record<string, { ok: boolean; text: string }>>({});
  const test = useMutation({
    mutationFn: (target: NotifyTarget) => service.test(targetRef(target)),
    onSuccess: async (result, target) => {
      const ref = targetRef(target);
      const text = result.delivered
        ? t('bucketEvents.testDelivered', { target: ref })
        : t('bucketEvents.testFailed', { target: ref, error: result.error ?? '' });
      setResults((current) => ({ ...current, [ref]: { ok: result.delivered, text } }));
      notifications.show({ color: result.delivered ? 'green' : 'red', message: text });
      await queryClient.invalidateQueries({ queryKey: storageKeys.notifyTargets });
    },
    onError: (error, target) => {
      const text = problemMessage(t, error);
      setResults((current) => ({ ...current, [targetRef(target)]: { ok: false, text } }));
    },
  });
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('bucketEvents.targetsTitle')}
    >
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Stack gap={2}>
            <Title order={4}>{t('bucketEvents.targetsTitle')}</Title>
            <Text size="sm" c="dimmed">
              {t('bucketEvents.targetsDescription')}
            </Text>
          </Stack>
          <Anchor component={Link} to="/configuration#notify" size="sm">
            {t('bucketEvents.manageTargets')}
          </Anchor>
        </Group>
        {targets.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('bucketEvents.noTargets')}
          </Text>
        ) : (
          <Table.ScrollContainer minWidth={560}>
            <Table aria-label={t('bucketEvents.targetsTitle')} verticalSpacing="sm">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('bucketEvents.columns.target')}</Table.Th>
                  <Table.Th>{t('bucketEvents.columns.state')}</Table.Th>
                  <Table.Th>{t('bucketEvents.columns.pending')}</Table.Th>
                  <Table.Th>{t('bucketEvents.columns.test')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {targets.map((target) => {
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
                        <Stack gap={2}>
                          <TargetState target={target} />
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
                          <Button
                            size="xs"
                            variant="default"
                            leftSection={<IconSend size={14} />}
                            disabled={!target.enabled}
                            loading={test.isPending && test.variables === target}
                            aria-label={t('bucketEvents.sendTestTo', { target: ref })}
                            onClick={() => {
                              test.mutate(target);
                            }}
                          >
                            {t('bucketEvents.sendTest')}
                          </Button>
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
    </Paper>
  );
}

/** D-BKT-04: event subscriptions of a bucket and the state of the targets. */
export function BucketEventsTab() {
  const { t } = useTranslation();
  const { bucket = '' } = useParams();
  const { notifications: service } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions({ bucket });
  const canEdit = perms.can(StorageActions.putNotification);
  const canSeeTargets = perms.can(StorageActions.configUpdate);
  const [editing, setEditing] = useState<number | null>(null);
  const [deleting, setDeleting] = useState<number | null>(null);
  const key = storageKeys.bucketSetting(bucket, 'events');
  const query = useQuery({ queryKey: key, queryFn: () => service.get(bucket) });
  const targets = useQuery({
    queryKey: storageKeys.notifyTargets,
    queryFn: () => service.targets(),
    enabled: canSeeTargets,
  });
  const subscriptions = query.data?.subscriptions ?? [];
  const knownTargets = canSeeTargets ? targets.data : undefined;

  const save = useMutation({
    mutationFn: async (next: EventSubscription[]) => {
      if (next.length === 0) {
        await service.remove(bucket);
        return { subscriptions: [] };
      }
      return service.set(bucket, { subscriptions: next });
    },
    onSuccess: (stored) => {
      queryClient.setQueryData(key, stored);
      setEditing(null);
      setDeleting(null);
      notifications.show({ color: 'green', message: t('bucketEvents.saved') });
    },
  });
  const saveWith = (index: number, sub: EventSubscription | null) => {
    save.mutate(
      sub === null
        ? subscriptions.filter((_, i) => i !== index)
        : index < 0
          ? [...subscriptions, sub]
          : subscriptions.map((s, i) => (i === index ? sub : s)),
    );
  };
  const editedIndex = editing === null ? -1 : editing < 0 ? subscriptions.length : editing;
  const problems = ruleProblemsOf(save.error).filter(
    (p) => p.rule === editedIndex + 1 || p.rule === 0,
  );

  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {isApiError(query.error) && query.error.isForbidden
          ? t('bucketEvents.unavailable')
          : problemMessage(t, query.error)}
      </Alert>
    );
  }

  return (
    <Stack gap="md">
      <Paper withBorder radius="md" p="md" component="section" aria-label={t('bucketEvents.title')}>
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
            <Stack gap={2}>
              <Title order={4}>{t('bucketEvents.title')}</Title>
              <Text size="sm" c="dimmed">
                {t('bucketEvents.description')}
              </Text>
            </Stack>
            <PermissionGuard allowed={canEdit} action={StorageActions.putNotification}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled || query.isPending}
                  onClick={() => {
                    save.reset();
                    setEditing(-1);
                  }}
                >
                  {t('bucketEvents.add')}
                </Button>
              )}
            </PermissionGuard>
          </Group>
          {query.isPending ? (
            <Skeleton height={100} />
          ) : subscriptions.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t('bucketEvents.empty')}
            </Text>
          ) : (
            <Table.ScrollContainer minWidth={640}>
              <Table aria-label={t('bucketEvents.subscriptions')} verticalSpacing="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('bucketEvents.columns.id')}</Table.Th>
                    <Table.Th>{t('bucketEvents.columns.target')}</Table.Th>
                    <Table.Th>{t('bucketEvents.columns.events')}</Table.Th>
                    <Table.Th>{t('bucketEvents.columns.filter')}</Table.Th>
                    <Table.Th w={90}>
                      <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                        {t('common.actions')}
                      </Text>
                    </Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {subscriptions.map((sub, index) => {
                    const id = sub.id ?? String(index + 1);
                    const target = knownTargets?.find((x) => x.arn === sub.arn);
                    return (
                      <Table.Tr key={id}>
                        <Table.Td>
                          <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                            {id}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <Group gap="xs" wrap="nowrap">
                            <Text size="sm" ff="monospace">
                              {targetLabel(sub.arn, knownTargets)}
                            </Text>
                            {target !== undefined ? <TargetState target={target} /> : null}
                          </Group>
                        </Table.Td>
                        <Table.Td>
                          <Group gap={4}>
                            {sub.events.map((e) => (
                              <Badge key={e} variant="outline" color="gray" tt="none">
                                {e}
                              </Badge>
                            ))}
                          </Group>
                        </Table.Td>
                        <Table.Td>
                          <Text size="sm">
                            {[
                              sub.prefix ? t('bucketEvents.prefixIs', { prefix: sub.prefix }) : '',
                              sub.suffix ? t('bucketEvents.suffixIs', { suffix: sub.suffix }) : '',
                            ]
                              .filter((s) => s !== '')
                              .join(' · ') || t('bucketEvents.everyObject')}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          {canEdit ? (
                            <Group gap={4} wrap="nowrap" justify="flex-end">
                              <Tooltip label={t('bucketEvents.edit', { id })}>
                                <ActionIcon
                                  variant="subtle"
                                  aria-label={t('bucketEvents.edit', { id })}
                                  onClick={() => {
                                    save.reset();
                                    setEditing(index);
                                  }}
                                >
                                  <IconEdit size={16} />
                                </ActionIcon>
                              </Tooltip>
                              <Tooltip label={t('bucketEvents.remove', { id })}>
                                <ActionIcon
                                  variant="subtle"
                                  color="red"
                                  aria-label={t('bucketEvents.remove', { id })}
                                  onClick={() => {
                                    save.reset();
                                    setDeleting(index);
                                  }}
                                >
                                  <IconTrash size={16} />
                                </ActionIcon>
                              </Tooltip>
                            </Group>
                          ) : null}
                        </Table.Td>
                      </Table.Tr>
                    );
                  })}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
          {!canSeeTargets ? (
            <Text size="xs" c="dimmed">
              {t('bucketEvents.targetsHidden')}
            </Text>
          ) : null}
        </Stack>
      </Paper>

      {canSeeTargets ? (
        targets.isPending ? (
          <Skeleton height={80} />
        ) : targets.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, targets.error)}
          </Alert>
        ) : (
          <TargetsSection targets={targets.data} />
        )
      ) : null}

      <SubscriptionEditor
        opened={editing !== null}
        subscription={editing === null || editing < 0 ? null : (subscriptions[editing] ?? null)}
        targets={knownTargets}
        saving={save.isPending}
        problems={editing !== null ? problems : []}
        error={
          editing !== null && save.isError && ruleProblemsOf(save.error).length === 0
            ? problemMessage(t, save.error)
            : undefined
        }
        onSave={(sub) => {
          if (editing !== null) saveWith(editing, sub);
        }}
        onClose={() => {
          setEditing(null);
          save.reset();
        }}
      />
      <ConfirmDialog
        opened={deleting !== null}
        title={t('bucketEvents.deleteTitle')}
        message={t('bucketEvents.deleteConfirm', {
          id: deleting === null ? '' : (subscriptions[deleting]?.id ?? String(deleting + 1)),
        })}
        confirmLabel={t('common.delete')}
        loading={save.isPending}
        error={deleting !== null && save.isError ? save.error : undefined}
        onCancel={() => {
          setDeleting(null);
          save.reset();
        }}
        onConfirm={() => {
          if (deleting !== null) saveWith(deleting, null);
        }}
      />
    </Stack>
  );
}
