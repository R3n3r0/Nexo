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
  Skeleton,
  Stack,
  Switch,
  Table,
  TagsInput,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconEdit, IconPlus, IconRefresh, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { formatBytes, formatNumber } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices, type ReplicationRule, type ReplicationTarget } from '@/services';

/** The name of a target in the tables: its label, else its endpoint and bucket. */
function targetName(target: ReplicationTarget | undefined, arn: string): string {
  if (target === undefined) return arn;
  return target.label ? target.label : `${target.endpoint}/${target.bucket}`;
}

/** Parses the "key=value" entries of the tags input. */
function parseTags(entries: readonly string[]): Record<string, string> | 'invalid' {
  const out: Record<string, string> = {};
  for (const entry of entries) {
    const at = entry.indexOf('=');
    if (at <= 0) return 'invalid';
    const key = entry.slice(0, at).trim();
    if (key === '' || key in out) return 'invalid';
    out[key] = entry.slice(at + 1);
  }
  return out;
}

interface TargetEditorProps {
  opened: boolean;
  target: ReplicationTarget | null;
  saving: boolean;
  error: string | undefined;
  onSave: (target: ReplicationTarget) => void;
  onClose: () => void;
}

/** Editor of a target: the remote S3 endpoint, bucket and credentials. */
function TargetEditor({ opened, target, saving, error, onSave, onClose }: TargetEditorProps) {
  const { t } = useTranslation();
  const [endpoint, setEndpoint] = useState('');
  const [bucket, setBucket] = useState('');
  const [region, setRegion] = useState('');
  const [accessKey, setAccessKey] = useState('');
  const [secretKey, setSecretKey] = useState('');
  const [label, setLabel] = useState('');
  const [submitted, setSubmitted] = useState(false);

  useEffect(() => {
    if (!opened) return;
    setEndpoint(target?.endpoint ?? '');
    setBucket(target?.bucket ?? '');
    setRegion(target?.region ?? '');
    setAccessKey(target?.accessKey ?? '');
    setSecretKey('');
    setLabel(target?.label ?? '');
    setSubmitted(false);
  }, [opened, target]);

  const editing = target !== null;
  const endpointError = !/^https?:\/\/[^/\s]+\/?$/.test(endpoint.trim())
    ? t('bucketReplication.endpointInvalid')
    : undefined;
  const bucketError = bucket.trim() === '' ? t('bucketReplication.required') : undefined;
  const accessKeyError = accessKey.trim() === '' ? t('bucketReplication.required') : undefined;
  const secretError = !editing && secretKey === '' ? t('bucketReplication.required') : undefined;
  const valid = !endpointError && !bucketError && !accessKeyError && !secretError;

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={editing ? t('bucketReplication.editTarget') : t('bucketReplication.addTarget')}
      centered
      size="lg"
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(true);
          if (!valid) return;
          onSave({
            ...(target?.arn ? { arn: target.arn } : {}),
            endpoint: endpoint.trim().replace(/\/+$/, ''),
            bucket: bucket.trim(),
            ...(region.trim() !== '' ? { region: region.trim() } : {}),
            accessKey: accessKey.trim(),
            ...(secretKey !== '' ? { secretKey } : {}),
            ...(label.trim() !== '' ? { label: label.trim() } : {}),
          });
        }}
      >
        <Stack gap="sm">
          <TextInput
            label={t('bucketReplication.endpoint')}
            description={t('bucketReplication.endpointHint')}
            placeholder="https://backup.example.com:9000"
            value={endpoint}
            disabled={editing}
            onChange={(e) => setEndpoint(e.currentTarget.value)}
            error={submitted ? endpointError : undefined}
            required
          />
          <TextInput
            label={t('bucketReplication.remoteBucket')}
            description={t('bucketReplication.remoteBucketHint')}
            value={bucket}
            disabled={editing}
            onChange={(e) => setBucket(e.currentTarget.value)}
            error={submitted ? bucketError : undefined}
            required
          />
          <TextInput
            label={t('bucketReplication.region')}
            value={region}
            onChange={(e) => setRegion(e.currentTarget.value)}
          />
          <TextInput
            label={t('bucketReplication.accessKey')}
            value={accessKey}
            autoComplete="off"
            onChange={(e) => setAccessKey(e.currentTarget.value)}
            error={submitted ? accessKeyError : undefined}
            required
          />
          <PasswordInput
            label={t('bucketReplication.secretKey')}
            description={
              editing ? t('bucketReplication.secretKeepHint') : t('bucketReplication.secretHint')
            }
            value={secretKey}
            autoComplete="new-password"
            onChange={(e) => setSecretKey(e.currentTarget.value)}
            error={submitted ? secretError : undefined}
            required={!editing}
          />
          <TextInput
            label={t('bucketReplication.label')}
            value={label}
            onChange={(e) => setLabel(e.currentTarget.value)}
          />
          {error ? (
            <Alert color="red" variant="light" role="alert">
              {error}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={saving}>
              {t('common.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

interface RuleEditorProps {
  opened: boolean;
  rule: ReplicationRule | null;
  targets: readonly ReplicationTarget[];
  /** The ids and priorities of the other rules (duplicates). */
  otherIds: readonly string[];
  otherPriorities: readonly number[];
  saving: boolean;
  error: string | undefined;
  onSave: (rule: ReplicationRule) => void;
  onClose: () => void;
}

/** Editor of a rule: the keys it selects, its target and what it replicates. */
function RuleEditor({
  opened,
  rule,
  targets,
  otherIds,
  otherPriorities,
  saving,
  error,
  onSave,
  onClose,
}: RuleEditorProps) {
  const { t } = useTranslation();
  const [id, setId] = useState('');
  const [priority, setPriority] = useState<number | string>(1);
  const [target, setTarget] = useState<string | null>(null);
  const [prefix, setPrefix] = useState('');
  const [tags, setTags] = useState<string[]>([]);
  const [enabled, setEnabled] = useState(true);
  const [deleteMarkers, setDeleteMarkers] = useState(true);
  const [deletes, setDeletes] = useState(false);
  const [existing, setExisting] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  useEffect(() => {
    if (!opened) return;
    setId(rule?.id ?? '');
    setPriority(rule?.priority ?? Math.max(0, ...otherPriorities) + 1);
    setTarget(rule?.target ?? targets[0]?.arn ?? null);
    setPrefix(rule?.prefix ?? '');
    setTags(Object.entries(rule?.tags ?? {}).map(([k, v]) => `${k}=${v}`));
    setEnabled(rule?.enabled ?? true);
    setDeleteMarkers(rule?.deleteMarkers ?? true);
    setDeletes(rule?.deletes ?? false);
    setExisting(rule?.existing ?? false);
    setSubmitted(false);
  }, [opened, rule, targets, otherPriorities]);

  const parsedTags = parseTags(tags);
  const idError =
    id.trim() === ''
      ? t('bucketReplication.required')
      : otherIds.includes(id.trim())
        ? t('bucketReplication.duplicateId')
        : undefined;
  const priorityNumber = typeof priority === 'number' ? priority : Number(priority);
  const priorityError =
    !Number.isInteger(priorityNumber) || priorityNumber < 0
      ? t('bucketReplication.priorityInvalid')
      : otherPriorities.includes(priorityNumber)
        ? t('bucketReplication.duplicatePriority')
        : undefined;
  const targetError = target === null ? t('bucketReplication.required') : undefined;
  const tagsError = parsedTags === 'invalid' ? t('bucketReplication.tagsInvalid') : undefined;
  const valid = !idError && !priorityError && !targetError && !tagsError;

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={rule === null ? t('bucketReplication.addRule') : t('bucketReplication.editRule')}
      centered
      size="lg"
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(true);
          if (!valid || target === null || parsedTags === 'invalid') return;
          onSave({
            id: id.trim(),
            priority: priorityNumber,
            enabled,
            target,
            ...(prefix !== '' ? { prefix } : {}),
            ...(Object.keys(parsedTags).length > 0 ? { tags: parsedTags } : {}),
            deleteMarkers,
            deletes,
            existing,
          });
        }}
      >
        <Stack gap="sm">
          <TextInput
            label={t('bucketReplication.ruleId')}
            value={id}
            onChange={(e) => setId(e.currentTarget.value)}
            error={submitted ? idError : undefined}
            required
          />
          <Select
            label={t('bucketReplication.target')}
            data={targets.map((x) => ({ value: x.arn ?? '', label: targetName(x, x.arn ?? '') }))}
            value={target}
            onChange={setTarget}
            error={submitted ? targetError : undefined}
            allowDeselect={false}
            required
          />
          <NumberInput
            label={t('bucketReplication.priority')}
            description={t('bucketReplication.priorityHint')}
            value={priority}
            onChange={setPriority}
            min={0}
            allowDecimal={false}
            error={submitted ? priorityError : undefined}
          />
          <TextInput
            label={t('bucketReplication.prefix')}
            description={t('bucketReplication.prefixHint')}
            value={prefix}
            onChange={(e) => setPrefix(e.currentTarget.value)}
          />
          <TagsInput
            label={t('bucketReplication.tags')}
            description={t('bucketReplication.tagsHint')}
            value={tags}
            onChange={setTags}
            error={submitted ? tagsError : undefined}
          />
          <Switch
            label={t('bucketReplication.enabled')}
            checked={enabled}
            onChange={(e) => setEnabled(e.currentTarget.checked)}
          />
          <Switch
            label={t('bucketReplication.deleteMarkers')}
            description={t('bucketReplication.deleteMarkersHint')}
            checked={deleteMarkers}
            onChange={(e) => setDeleteMarkers(e.currentTarget.checked)}
          />
          <Switch
            label={t('bucketReplication.deletes')}
            description={t('bucketReplication.deletesHint')}
            checked={deletes}
            onChange={(e) => setDeletes(e.currentTarget.checked)}
          />
          <Switch
            label={t('bucketReplication.existing')}
            description={t('bucketReplication.existingHint')}
            checked={existing}
            onChange={(e) => setExisting(e.currentTarget.checked)}
          />
          {error ? (
            <Alert color="red" variant="light" role="alert">
              {error}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={saving}>
              {t('common.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/** What a rule selects, in one line. */
function useSelection() {
  const { t } = useTranslation();
  return (rule: ReplicationRule) => {
    const parts: string[] = [];
    if (rule.prefix) parts.push(t('bucketReplication.prefixIs', { prefix: rule.prefix }));
    for (const [k, v] of Object.entries(rule.tags ?? {})) parts.push(`${k}=${v}`);
    return parts.length > 0 ? parts.join(', ') : t('bucketReplication.wholeBucket');
  };
}

/**
 * Replication of a bucket (D-BKT-05, S-BKT-12): the remote buckets it
 * replicates to, the rules that select what goes to each one, and the
 * state of the replication with the resync of the existing versions.
 */
export function BucketReplicationTab() {
  const { t, i18n } = useTranslation();
  const { bucket = '' } = useParams();
  const { replication } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions({ bucket });
  const canRead = !perms.ready || perms.can(StorageActions.getReplication);
  const canReadTargets = !perms.ready || perms.can(StorageActions.getBucketTarget);
  const canEditRules = perms.can(StorageActions.putReplication);
  const canEditTargets = perms.can(StorageActions.setBucketTarget);
  const canResync = perms.can(StorageActions.resyncReplication);
  const selection = useSelection();

  const rulesKey = storageKeys.bucketSetting(bucket, 'replication');
  const targetsKey = storageKeys.bucketSetting(bucket, 'replicationTargets');
  const statusKey = storageKeys.bucketSetting(bucket, 'replicationStatus');
  const rulesQuery = useQuery({
    queryKey: rulesKey,
    queryFn: () => replication.get(bucket),
    enabled: canRead,
  });
  const targetsQuery = useQuery({
    queryKey: targetsKey,
    queryFn: () => replication.targets(bucket),
    enabled: canReadTargets,
  });
  const statusQuery = useQuery({
    queryKey: statusKey,
    queryFn: () => replication.status(bucket),
    enabled: canRead,
    refetchInterval: (q) => (q.state.data?.resync?.state === 'running' ? 2000 : false),
  });
  const rules = rulesQuery.data?.rules ?? [];
  const targets = targetsQuery.data ?? [];
  const targetOf = (arn: string) => targets.find((x) => x.arn === arn);

  const [editingRule, setEditingRule] = useState<number | null>(null);
  const [deletingRule, setDeletingRule] = useState<number | null>(null);
  const [editingTarget, setEditingTarget] = useState<ReplicationTarget | 'new' | null>(null);
  const [deletingTarget, setDeletingTarget] = useState<ReplicationTarget | null>(null);

  const saveRules = useMutation({
    mutationFn: async (next: ReplicationRule[]) => {
      if (next.length === 0) {
        await replication.remove(bucket);
        return { rules: [] };
      }
      return replication.set(bucket, { rules: next });
    },
    onSuccess: (stored) => {
      queryClient.setQueryData(rulesKey, stored);
      void queryClient.invalidateQueries({ queryKey: statusKey });
      setEditingRule(null);
      setDeletingRule(null);
      notifications.show({ color: 'green', message: t('bucketReplication.saved') });
    },
  });
  const saveTarget = useMutation({
    mutationFn: (target: ReplicationTarget) => replication.setTarget(bucket, target),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: targetsKey });
      void queryClient.invalidateQueries({ queryKey: statusKey });
      setEditingTarget(null);
      notifications.show({ color: 'green', message: t('bucketReplication.targetSaved') });
    },
  });
  const removeTarget = useMutation({
    mutationFn: (arn: string) => replication.removeTarget(bucket, arn),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: targetsKey });
      void queryClient.invalidateQueries({ queryKey: statusKey });
      setDeletingTarget(null);
      notifications.show({ color: 'green', message: t('bucketReplication.targetRemoved') });
    },
  });
  const resync = useMutation({
    mutationFn: (arn: string) => replication.resync(bucket, arn),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: statusKey });
      notifications.show({ color: 'green', message: t('bucketReplication.resyncStarted') });
    },
    onError: (error) => {
      notifications.show({ color: 'red', message: problemMessage(t, error) });
    },
  });

  const saveRuleAt = (index: number, rule: ReplicationRule | null) => {
    saveRules.mutate(
      rule === null
        ? rules.filter((_, i) => i !== index)
        : index < 0
          ? [...rules, rule]
          : rules.map((r, i) => (i === index ? rule : r)),
    );
  };

  if (
    !canRead ||
    (rulesQuery.isError && isApiError(rulesQuery.error) && rulesQuery.error.isForbidden)
  ) {
    return (
      <Alert color="red" variant="light" role="alert">
        {t('bucketReplication.unavailable')}
      </Alert>
    );
  }
  if (rulesQuery.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {problemMessage(t, rulesQuery.error)}
      </Alert>
    );
  }

  const status = statusQuery.data;
  const editedRule = editingRule === null || editingRule < 0 ? null : (rules[editingRule] ?? null);
  const others = rules.filter((_, i) => i !== editingRule);

  return (
    <Stack gap="md">
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('bucketReplication.targetsTitle')}
      >
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
            <Stack gap={2}>
              <Title order={4}>{t('bucketReplication.targetsTitle')}</Title>
              <Text size="sm" c="dimmed">
                {t('bucketReplication.targetsDescription')}
              </Text>
            </Stack>
            <PermissionGuard allowed={canEditTargets} action={StorageActions.setBucketTarget}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled}
                  onClick={() => {
                    saveTarget.reset();
                    setEditingTarget('new');
                  }}
                >
                  {t('bucketReplication.addTarget')}
                </Button>
              )}
            </PermissionGuard>
          </Group>
          {!canReadTargets ? (
            <Text size="sm" c="dimmed">
              {t('bucketReplication.targetsUnavailable')}
            </Text>
          ) : targetsQuery.isPending ? (
            <Skeleton height={60} />
          ) : targets.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t('bucketReplication.noTargets')}
            </Text>
          ) : (
            <Table.ScrollContainer minWidth={640}>
              <Table aria-label={t('bucketReplication.targetsTitle')} verticalSpacing="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('bucketReplication.columns.target')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.remote')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.accessKey')}</Table.Th>
                    <Table.Th w={90}>
                      <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                        {t('common.actions')}
                      </Text>
                    </Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {targets.map((target) => (
                    <Table.Tr key={target.arn}>
                      <Table.Td>
                        <Stack gap={0}>
                          <Text size="sm">{target.label || target.bucket}</Text>
                          <Text
                            size="xs"
                            c="dimmed"
                            ff="monospace"
                            style={{ overflowWrap: 'anywhere' }}
                          >
                            {target.arn}
                          </Text>
                        </Stack>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                          {`${target.endpoint}/${target.bucket}`}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" ff="monospace">
                          {target.accessKey}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        {canEditTargets ? (
                          <Group gap={4} wrap="nowrap" justify="flex-end">
                            <Tooltip
                              label={t('bucketReplication.editTargetOf', {
                                name: targetName(target, ''),
                              })}
                            >
                              <ActionIcon
                                variant="subtle"
                                aria-label={t('bucketReplication.editTargetOf', {
                                  name: targetName(target, ''),
                                })}
                                onClick={() => {
                                  saveTarget.reset();
                                  setEditingTarget(target);
                                }}
                              >
                                <IconEdit size={16} />
                              </ActionIcon>
                            </Tooltip>
                            <Tooltip
                              label={t('bucketReplication.removeTargetOf', {
                                name: targetName(target, ''),
                              })}
                            >
                              <ActionIcon
                                variant="subtle"
                                color="red"
                                aria-label={t('bucketReplication.removeTargetOf', {
                                  name: targetName(target, ''),
                                })}
                                onClick={() => {
                                  removeTarget.reset();
                                  setDeletingTarget(target);
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
      </Paper>

      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('bucketReplication.rulesTitle')}
      >
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
            <Stack gap={2}>
              <Title order={4}>{t('bucketReplication.rulesTitle')}</Title>
              <Text size="sm" c="dimmed">
                {t('bucketReplication.rulesDescription')}
              </Text>
            </Stack>
            <PermissionGuard allowed={canEditRules} action={StorageActions.putReplication}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled || rulesQuery.isPending || targets.length === 0}
                  onClick={() => {
                    saveRules.reset();
                    setEditingRule(-1);
                  }}
                >
                  {t('bucketReplication.addRule')}
                </Button>
              )}
            </PermissionGuard>
          </Group>
          {rulesQuery.isPending ? (
            <Skeleton height={80} />
          ) : rules.length === 0 ? (
            <Text size="sm" c="dimmed">
              {targets.length === 0
                ? t('bucketReplication.needTarget')
                : t('bucketReplication.noRules')}
            </Text>
          ) : (
            <Table.ScrollContainer minWidth={720}>
              <Table aria-label={t('bucketReplication.rulesTitle')} verticalSpacing="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('bucketReplication.columns.rule')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.target')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.selects')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.replicates')}</Table.Th>
                    <Table.Th w={90}>
                      <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                        {t('common.actions')}
                      </Text>
                    </Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {rules.map((rule, index) => (
                    <Table.Tr key={rule.id}>
                      <Table.Td>
                        <Stack gap={2}>
                          <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                            {rule.id}
                          </Text>
                          <Group gap={4}>
                            <Badge
                              size="xs"
                              variant="light"
                              color={rule.enabled ? 'green' : 'gray'}
                            >
                              {rule.enabled
                                ? t('bucketReplication.on')
                                : t('bucketReplication.off')}
                            </Badge>
                            <Text size="xs" c="dimmed">
                              {t('bucketReplication.priorityIs', { priority: rule.priority })}
                            </Text>
                          </Group>
                        </Stack>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                          {targetName(targetOf(rule.target), rule.target)}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                          {selection(rule)}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Group gap={4}>
                          <Badge size="sm" variant="light">
                            {t('bucketReplication.versions')}
                          </Badge>
                          {rule.deleteMarkers ? (
                            <Badge size="sm" variant="light">
                              {t('bucketReplication.deleteMarkersBadge')}
                            </Badge>
                          ) : null}
                          {rule.deletes ? (
                            <Badge size="sm" variant="light" color="orange">
                              {t('bucketReplication.deletesBadge')}
                            </Badge>
                          ) : null}
                          {rule.existing ? (
                            <Badge size="sm" variant="light" color="grape">
                              {t('bucketReplication.existingBadge')}
                            </Badge>
                          ) : null}
                        </Group>
                      </Table.Td>
                      <Table.Td>
                        {canEditRules ? (
                          <Group gap={4} wrap="nowrap" justify="flex-end">
                            <Tooltip label={t('bucketReplication.editRuleOf', { id: rule.id })}>
                              <ActionIcon
                                variant="subtle"
                                aria-label={t('bucketReplication.editRuleOf', { id: rule.id })}
                                onClick={() => {
                                  saveRules.reset();
                                  setEditingRule(index);
                                }}
                              >
                                <IconEdit size={16} />
                              </ActionIcon>
                            </Tooltip>
                            <Tooltip label={t('bucketReplication.removeRuleOf', { id: rule.id })}>
                              <ActionIcon
                                variant="subtle"
                                color="red"
                                aria-label={t('bucketReplication.removeRuleOf', { id: rule.id })}
                                onClick={() => {
                                  saveRules.reset();
                                  setDeletingRule(index);
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
          <Text size="xs" c="dimmed">
            {t('bucketReplication.activeActive')}
          </Text>
        </Stack>
      </Paper>

      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('bucketReplication.statusTitle')}
      >
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
            <Stack gap={2}>
              <Title order={4}>{t('bucketReplication.statusTitle')}</Title>
              <Text size="sm" c="dimmed">
                {t('bucketReplication.statusDescription')}
              </Text>
            </Stack>
            <Button
              variant="default"
              leftSection={<IconRefresh size={16} />}
              loading={statusQuery.isFetching}
              onClick={() => void statusQuery.refetch()}
            >
              {t('bucketReplication.refresh')}
            </Button>
          </Group>
          {statusQuery.isPending ? (
            <Skeleton height={60} />
          ) : statusQuery.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, statusQuery.error)}
            </Alert>
          ) : (status?.targets.length ?? 0) === 0 ? (
            <Text size="sm" c="dimmed">
              {t('bucketReplication.noTargets')}
            </Text>
          ) : (
            <Table.ScrollContainer minWidth={720}>
              <Table aria-label={t('bucketReplication.statusTitle')} verticalSpacing="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('bucketReplication.columns.target')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.state')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.queued')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.replicated')}</Table.Th>
                    <Table.Th>{t('bucketReplication.columns.failed')}</Table.Th>
                    <Table.Th w={130}>
                      <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                        {t('common.actions')}
                      </Text>
                    </Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {status?.targets.map((s) => {
                    const usable = rules.some((r) => r.target === s.arn && r.existing);
                    return (
                      <Table.Tr key={s.arn}>
                        <Table.Td>
                          <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                            {targetName(targetOf(s.arn), s.arn)}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <Badge variant="light" color={s.online ? 'green' : 'red'}>
                            {s.online
                              ? t('bucketReplication.online')
                              : t('bucketReplication.offline')}
                          </Badge>
                        </Table.Td>
                        <Table.Td>{formatNumber(s.queued, i18n.language)}</Table.Td>
                        <Table.Td>
                          {t('bucketReplication.replicatedIs', {
                            count: s.replicated,
                            versions: formatNumber(s.replicated, i18n.language),
                            size: formatBytes(s.bytes, i18n.language),
                          })}
                        </Table.Td>
                        <Table.Td>
                          <Stack gap={0}>
                            <Text size="sm">{formatNumber(s.failed, i18n.language)}</Text>
                            {s.lastError ? (
                              <Text size="xs" c="red" style={{ overflowWrap: 'anywhere' }}>
                                {s.lastError}
                              </Text>
                            ) : null}
                          </Stack>
                        </Table.Td>
                        <Table.Td>
                          {canResync ? (
                            <Tooltip
                              label={
                                usable
                                  ? t('bucketReplication.resyncHint')
                                  : t('bucketReplication.resyncNeedsExisting')
                              }
                            >
                              <Button
                                size="xs"
                                variant="light"
                                disabled={!usable || status.resync?.state === 'running'}
                                loading={resync.isPending && resync.variables === s.arn}
                                onClick={() => resync.mutate(s.arn)}
                              >
                                {t('bucketReplication.resync')}
                              </Button>
                            </Tooltip>
                          ) : null}
                        </Table.Td>
                      </Table.Tr>
                    );
                  })}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
          {status?.resync ? (
            <Alert
              variant="light"
              color={
                status.resync.state === 'failed'
                  ? 'red'
                  : status.resync.state === 'complete'
                    ? 'green'
                    : 'blue'
              }
              role="status"
            >
              {t(`bucketReplication.resyncStates.${status.resync.state ?? 'running'}`, {
                target: targetName(targetOf(status.resync.target), status.resync.target),
                scanned: formatNumber(status.resync.scanned ?? 0, i18n.language),
                queued: formatNumber(status.resync.queued ?? 0, i18n.language),
              })}
              {status.resync.error ? ` ${status.resync.error}` : ''}
            </Alert>
          ) : null}
          <Text size="xs" c="dimmed">
            {t('bucketReplication.perNode')}
          </Text>
        </Stack>
      </Paper>

      <TargetEditor
        opened={editingTarget !== null}
        target={editingTarget === 'new' ? null : editingTarget}
        saving={saveTarget.isPending}
        error={saveTarget.isError ? problemMessage(t, saveTarget.error) : undefined}
        onSave={(target) => saveTarget.mutate(target)}
        onClose={() => {
          setEditingTarget(null);
          saveTarget.reset();
        }}
      />
      <RuleEditor
        opened={editingRule !== null}
        rule={editedRule}
        targets={targets}
        otherIds={others.map((r) => r.id)}
        otherPriorities={others.map((r) => r.priority)}
        saving={saveRules.isPending}
        error={
          editingRule !== null && saveRules.isError ? problemMessage(t, saveRules.error) : undefined
        }
        onSave={(rule) => {
          if (editingRule !== null) saveRuleAt(editingRule, rule);
        }}
        onClose={() => {
          setEditingRule(null);
          saveRules.reset();
        }}
      />
      <ConfirmDialog
        opened={deletingRule !== null}
        title={t('bucketReplication.deleteRuleTitle')}
        message={t('bucketReplication.deleteRuleConfirm', {
          id: deletingRule === null ? '' : (rules[deletingRule]?.id ?? ''),
        })}
        confirmLabel={t('common.delete')}
        loading={saveRules.isPending}
        error={deletingRule !== null && saveRules.isError ? saveRules.error : undefined}
        onCancel={() => {
          setDeletingRule(null);
          saveRules.reset();
        }}
        onConfirm={() => {
          if (deletingRule !== null) saveRuleAt(deletingRule, null);
        }}
      />
      <ConfirmDialog
        opened={deletingTarget !== null}
        title={t('bucketReplication.deleteTargetTitle')}
        message={t('bucketReplication.deleteTargetConfirm', {
          name: deletingTarget === null ? '' : targetName(deletingTarget, ''),
        })}
        confirmLabel={t('common.delete')}
        loading={removeTarget.isPending}
        error={removeTarget.isError ? removeTarget.error : undefined}
        onCancel={() => {
          setDeletingTarget(null);
          removeTarget.reset();
        }}
        onConfirm={() => {
          if (deletingTarget?.arn) removeTarget.mutate(deletingTarget.arn);
        }}
      />
    </Stack>
  );
}
