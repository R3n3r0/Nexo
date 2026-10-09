import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Paper,
  SegmentedControl,
  Skeleton,
  Stack,
  Switch,
  Table,
  Text,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconEdit, IconPlayerPlay, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { CodeEditor } from '@/components/code/CodeEditor';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { LifecycleRuleEditor } from '@/components/lifecycle/LifecycleRuleEditor';
import { formatBytes } from '@/lib/format';
import {
  lifecycleFromJson,
  lifecycleFromXml,
  lifecycleToJson,
  lifecycleToXml,
  scopeOf,
} from '@/lib/lifecycle';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import {
  isApiError,
  useServices,
  type LifecycleApplyResult,
  type LifecycleRule,
  type RuleProblem,
} from '@/services';

type View = 'rules' | 'json' | 'xml';

/** The rule errors of a failed save, or none. */
function ruleProblemsOf(error: unknown): readonly RuleProblem[] {
  return isApiError(error) ? error.ruleErrors : [];
}

/** The scope of a rule in words. */
function ScopeText({ rule }: { rule: LifecycleRule }) {
  const { t, i18n } = useTranslation();
  const scope = scopeOf(rule);
  const locale = i18n.resolvedLanguage;
  const parts = [
    ...(scope.prefix !== undefined ? [t('lifecycle.scope.prefix', { prefix: scope.prefix })] : []),
    ...(scope.tags.length > 0 ? [t('lifecycle.scope.tags', { tags: scope.tags.join(', ') })] : []),
    ...(scope.sizeGreaterThan !== undefined
      ? [t('lifecycle.scope.sizeGreater', { size: formatBytes(scope.sizeGreaterThan, locale) })]
      : []),
    ...(scope.sizeLessThan !== undefined
      ? [t('lifecycle.scope.sizeLess', { size: formatBytes(scope.sizeLessThan, locale) })]
      : []),
  ];
  return (
    <Text size="sm">
      {parts.length === 0 ? t('lifecycle.scope.wholeBucket') : parts.join(' · ')}
    </Text>
  );
}

/** The actions of a rule in words, one per line. */
function ActionsText({ rule }: { rule: LifecycleRule }) {
  const { t, i18n } = useTranslation();
  const lines: string[] = [];
  const exp = rule.expiration;
  if (exp?.expiredObjectDeleteMarker === true) {
    lines.push(t('lifecycle.summary.deleteMarkers'));
  } else if (exp?.date !== undefined && exp.date !== '') {
    const date = new Date(`${exp.date.slice(0, 10)}T00:00:00Z`);
    lines.push(
      t('lifecycle.summary.expireDate', {
        date: date.toLocaleDateString(i18n.resolvedLanguage, { timeZone: 'UTC' }),
      }),
    );
  } else if (exp?.days !== undefined) {
    lines.push(t('lifecycle.summary.expireDays', { count: exp.days }));
  }
  const nc = rule.noncurrentExpiration;
  if (nc !== undefined) {
    lines.push(
      nc.newerNoncurrentVersions !== undefined
        ? t('lifecycle.summary.noncurrentKeep', {
            count: nc.noncurrentDays ?? 0,
            keep: nc.newerNoncurrentVersions,
          })
        : t('lifecycle.summary.noncurrent', { count: nc.noncurrentDays ?? 0 }),
    );
  }
  if (rule.abortIncompleteUpload?.daysAfterInitiation !== undefined) {
    lines.push(
      t('lifecycle.summary.abort', { count: rule.abortIncompleteUpload.daysAfterInitiation }),
    );
  }
  for (const tr of [...(rule.transitions ?? []), ...(rule.noncurrentTransitions ?? [])]) {
    lines.push(t('lifecycle.summary.transition', { storageClass: tr.storageClass }));
  }
  return (
    <Stack gap={0}>
      {lines.map((line) => (
        <Text key={line} size="sm">
          {line}
        </Text>
      ))}
    </Stack>
  );
}

/** The refused rules of a save, as the server listed them. */
function RuleProblems({ problems }: { problems: readonly RuleProblem[] }) {
  const { t } = useTranslation();
  if (problems.length === 0) return null;
  return (
    <Alert color="red" variant="light" role="alert" title={t('lifecycle.serverRefused')}>
      <Stack gap={2}>
        {problems.map((p) => (
          <Text key={`${String(p.rule)}-${p.code}-${p.message}`} size="sm">
            {p.rule === 0
              ? p.message
              : t('lifecycle.ruleProblem', {
                  n: p.rule,
                  id: p.id !== undefined && p.id !== '' ? ` (${p.id})` : '',
                  message: p.message,
                })}
          </Text>
        ))}
      </Stack>
    </Alert>
  );
}

/** JSON or XML view of the whole configuration, editable and saved as a whole. */
function CodeView({
  view,
  rules,
  canEdit,
  saving,
  saveError,
  onSave,
}: {
  view: 'json' | 'xml';
  rules: LifecycleRule[];
  canEdit: boolean;
  saving: boolean;
  saveError: unknown;
  onSave: (rules: LifecycleRule[]) => void;
}) {
  const { t } = useTranslation();
  const serialized = view === 'json' ? lifecycleToJson({ rules }) : lifecycleToXml({ rules });
  const [draft, setDraft] = useState<string | null>(null);
  const text = draft ?? serialized;
  const parsed = view === 'json' ? lifecycleFromJson(text) : lifecycleFromXml(text);
  return (
    <Stack gap="sm">
      <CodeEditor
        label={view === 'json' ? t('lifecycle.code.jsonLabel') : t('lifecycle.code.xmlLabel')}
        description={t('lifecycle.code.hint')}
        language={view}
        value={text}
        height={360}
        readOnly={!canEdit}
        onChange={setDraft}
        error={parsed.ok ? undefined : parsed.error}
      />
      {saveError !== null && saveError !== undefined ? (
        ruleProblemsOf(saveError).length > 0 ? (
          <RuleProblems problems={ruleProblemsOf(saveError)} />
        ) : (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, saveError)}
          </Alert>
        )
      ) : null}
      {canEdit ? (
        <Group justify="flex-end" gap="xs">
          <Button
            variant="default"
            disabled={draft === null}
            onClick={() => {
              setDraft(null);
            }}
          >
            {t('lifecycle.code.reset')}
          </Button>
          <Button
            disabled={draft === null || !parsed.ok}
            loading={saving}
            onClick={() => {
              if (parsed.ok) {
                onSave(parsed.config.rules);
              }
            }}
          >
            {t('lifecycle.code.save')}
          </Button>
        </Group>
      ) : null}
    </Stack>
  );
}

/** D-BKT-03: lifecycle rules of a bucket, guided editor and code views. */
export function BucketLifecycleTab() {
  const { t } = useTranslation();
  const { bucket = '' } = useParams();
  const { lifecycle } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions({ bucket });
  const canEdit = perms.can(StorageActions.putLifecycle);
  const canApply = canEdit && perms.can(StorageActions.applyLifecycle);
  const [view, setView] = useState<View>('rules');
  // The rule in the editor: its index, or -1 for a new rule.
  const [editing, setEditing] = useState<number | null>(null);
  const [deleting, setDeleting] = useState<number | null>(null);
  const [applying, setApplying] = useState(false);
  const key = storageKeys.bucketSetting(bucket, 'lifecycle');
  const query = useQuery({ queryKey: key, queryFn: () => lifecycle.get(bucket) });
  const rules = query.data?.rules ?? [];

  const save = useMutation({
    mutationFn: async (next: LifecycleRule[]) => {
      if (next.length === 0) {
        await lifecycle.remove(bucket);
        return { rules: [] };
      }
      return lifecycle.set(bucket, { rules: next });
    },
    onSuccess: (stored) => {
      queryClient.setQueryData(key, stored);
      setEditing(null);
      setDeleting(null);
      notifications.show({ color: 'green', message: t('lifecycle.saved') });
    },
  });
  const apply = useMutation({
    mutationFn: () => lifecycle.apply(bucket),
    onSuccess: async (result: LifecycleApplyResult) => {
      setApplying(false);
      await queryClient.invalidateQueries({ queryKey: storageKeys.objects(bucket) });
      await queryClient.invalidateQueries({ queryKey: storageKeys.bucket(bucket) });
      notifications.show({
        color: 'green',
        title: t('lifecycle.applied'),
        message: t('lifecycle.appliedDetail', {
          versions: result.versions,
          deleted: result.deleted,
          markers: result.deleteMarkers,
          uploads: result.uploadsAborted,
          locked: result.locked,
        }),
      });
    },
  });

  const saveWith = (index: number, rule: LifecycleRule | null) => {
    const next =
      rule === null
        ? rules.filter((_, i) => i !== index)
        : index < 0
          ? [...rules, rule]
          : rules.map((r, i) => (i === index ? rule : r));
    save.mutate(next);
  };

  // The problems of the rule in the editor (its index in what was sent).
  const editedIndex = editing === null ? -1 : editing < 0 ? rules.length : editing;
  const problems = ruleProblemsOf(save.error);
  const editorProblems = problems.filter((p) => p.rule === editedIndex + 1 || p.rule === 0);
  const editorError =
    save.isError && problems.length === 0 ? problemMessage(t, save.error) : undefined;

  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {isApiError(query.error) && query.error.isForbidden
          ? t('lifecycle.unavailable')
          : problemMessage(t, query.error)}
      </Alert>
    );
  }

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('lifecycle.title')}>
      <Stack gap="md">
        <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
          <Stack gap={2}>
            <Title order={4}>{t('lifecycle.title')}</Title>
            <Text size="sm" c="dimmed">
              {t('lifecycle.description')}
            </Text>
          </Stack>
          <Group gap="xs">
            <PermissionGuard allowed={canApply} action={StorageActions.applyLifecycle}>
              {(disabled) => (
                <Button
                  variant="default"
                  leftSection={<IconPlayerPlay size={16} />}
                  disabled={disabled || rules.length === 0}
                  onClick={() => {
                    apply.reset();
                    setApplying(true);
                  }}
                >
                  {t('lifecycle.applyNow')}
                </Button>
              )}
            </PermissionGuard>
            <PermissionGuard allowed={canEdit} action={StorageActions.putLifecycle}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled || query.isPending}
                  onClick={() => {
                    save.reset();
                    setEditing(-1);
                  }}
                >
                  {t('lifecycle.add')}
                </Button>
              )}
            </PermissionGuard>
          </Group>
        </Group>
        <SegmentedControl
          aria-label={t('lifecycle.viewAs')}
          value={view}
          onChange={(next) => {
            save.reset();
            setView(next as View);
          }}
          data={[
            { value: 'rules', label: t('lifecycle.views.rules') },
            { value: 'json', label: 'JSON' },
            { value: 'xml', label: 'XML' },
          ]}
          style={{ alignSelf: 'flex-start' }}
        />
        {query.isPending ? (
          <Skeleton height={120} />
        ) : view === 'rules' ? (
          rules.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t('lifecycle.empty')}
            </Text>
          ) : (
            <Stack gap="sm">
              {editing === null && deleting === null ? <RuleProblems problems={problems} /> : null}
              <Table.ScrollContainer minWidth={640}>
                <Table aria-label={t('lifecycle.rules')} verticalSpacing="sm">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>{t('lifecycle.columns.id')}</Table.Th>
                      <Table.Th>{t('lifecycle.columns.status')}</Table.Th>
                      <Table.Th>{t('lifecycle.columns.scope')}</Table.Th>
                      <Table.Th>{t('lifecycle.columns.actions')}</Table.Th>
                      <Table.Th w={100}>
                        <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                          {t('lifecycle.columns.edit')}
                        </Text>
                      </Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {rules.map((rule, index) => {
                      const id = rule.id ?? String(index + 1);
                      return (
                        <Table.Tr key={id}>
                          <Table.Td>
                            <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                              {id}
                            </Text>
                          </Table.Td>
                          <Table.Td>
                            <Group gap="xs" wrap="nowrap">
                              <Switch
                                size="xs"
                                aria-label={t('lifecycle.toggle', { id })}
                                checked={rule.enabled !== false}
                                disabled={!canEdit || save.isPending}
                                onChange={(event) => {
                                  saveWith(index, {
                                    ...rule,
                                    enabled: event.currentTarget.checked,
                                  });
                                }}
                              />
                              <Badge
                                variant="light"
                                color={rule.enabled !== false ? 'green' : 'gray'}
                              >
                                {rule.enabled !== false
                                  ? t('lifecycle.status.enabled')
                                  : t('lifecycle.status.disabled')}
                              </Badge>
                            </Group>
                          </Table.Td>
                          <Table.Td>
                            <ScopeText rule={rule} />
                          </Table.Td>
                          <Table.Td>
                            <ActionsText rule={rule} />
                          </Table.Td>
                          <Table.Td>
                            <Group gap={4} wrap="nowrap" justify="flex-end">
                              <Tooltip
                                label={
                                  canEdit
                                    ? t('lifecycle.edit', { id })
                                    : t('lifecycle.view', { id })
                                }
                              >
                                <ActionIcon
                                  variant="subtle"
                                  aria-label={
                                    canEdit
                                      ? t('lifecycle.edit', { id })
                                      : t('lifecycle.view', { id })
                                  }
                                  onClick={() => {
                                    save.reset();
                                    setEditing(index);
                                  }}
                                >
                                  <IconEdit size={16} />
                                </ActionIcon>
                              </Tooltip>
                              {canEdit ? (
                                <Tooltip label={t('lifecycle.remove', { id })}>
                                  <ActionIcon
                                    variant="subtle"
                                    color="red"
                                    aria-label={t('lifecycle.remove', { id })}
                                    onClick={() => {
                                      save.reset();
                                      setDeleting(index);
                                    }}
                                  >
                                    <IconTrash size={16} />
                                  </ActionIcon>
                                </Tooltip>
                              ) : null}
                            </Group>
                          </Table.Td>
                        </Table.Tr>
                      );
                    })}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            </Stack>
          )
        ) : (
          <CodeView
            key={view}
            view={view}
            rules={rules}
            canEdit={canEdit}
            saving={save.isPending}
            saveError={editing === null ? save.error : null}
            onSave={(next) => {
              save.mutate(next);
            }}
          />
        )}
      </Stack>

      <LifecycleRuleEditor
        opened={editing !== null}
        rule={editing === null || editing < 0 ? null : (rules[editing] ?? null)}
        otherIds={rules
          .filter((_, i) => i !== editing)
          .map((r) => r.id ?? '')
          .filter((id) => id !== '')}
        saving={save.isPending}
        serverProblems={editing !== null ? editorProblems : []}
        serverError={editing !== null ? editorError : undefined}
        readOnly={!canEdit}
        onSave={(rule) => {
          if (editing !== null) saveWith(editing, rule);
        }}
        onClose={() => {
          setEditing(null);
          save.reset();
        }}
      />
      <ConfirmDialog
        opened={deleting !== null}
        title={t('lifecycle.deleteTitle')}
        message={t('lifecycle.deleteConfirm', {
          id: deleting === null ? '' : (rules[deleting]?.id ?? String(deleting + 1)),
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
      <ConfirmDialog
        opened={applying}
        title={t('lifecycle.applyTitle')}
        message={t('lifecycle.applyConfirm', { bucket })}
        confirmLabel={t('lifecycle.applyNow')}
        danger
        loading={apply.isPending}
        error={apply.isError ? apply.error : undefined}
        onCancel={() => {
          setApplying(false);
        }}
        onConfirm={() => {
          apply.mutate();
        }}
      />
    </Paper>
  );
}
