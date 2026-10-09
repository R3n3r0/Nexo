import {
  Alert,
  Badge,
  Button,
  Group,
  List,
  Paper,
  SegmentedControl,
  SimpleGrid,
  Skeleton,
  Stack,
  Text,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconAlertTriangle, IconPlus, IconRefresh, IconWorld } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { BlockPublicControl } from '@/components/accessRules/BlockPublicControl';
import { PresetPanel } from '@/components/accessRules/PresetPanel';
import { RuleEditor } from '@/components/accessRules/RuleEditor';
import { RulesCodeView } from '@/components/accessRules/RulesCodeView';
import { RulesTable } from '@/components/accessRules/RulesTable';
import { RuleTester } from '@/components/accessRules/RuleTester';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { accessKeys } from '@/lib/accessQueries';
import { moveRule, newRule, orderedRules } from '@/lib/accessRules';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import {
  isApiError,
  useServices,
  type AccessPresetInput,
  type AccessRuleProblem,
  type AccessRules,
  type AccessRulesKeyResult,
  type BucketAccessRule,
} from '@/services';

type View = 'rules' | 'yaml' | 'json';

/** The rule being edited: its id in the stored rules, or none for a new rule. */
interface Editing {
  id: string | null;
  rule: BucketAccessRule;
}

/** What was sent by the last save, to map the located errors back to a rule. */
interface Sent {
  rules: BucketAccessRule[];
  /** The id of the rule edited in the editor, if any. */
  edited: string | null;
}

function problemsOf(error: unknown): readonly AccessRuleProblem[] {
  return isApiError(error) ? error.accessRuleErrors : [];
}

/**
 * D-ACC-01..04: the access rules of a bucket. The table (ordered by
 * priority, reordered by drag and drop), the editor with its live pattern
 * preview, the YAML/JSON views, the public presets, the block public
 * access of the bucket and the tester. Every change replaces the rules
 * atomically with If-Match on the etag read: a concurrent change (412) is
 * reported and the rules reloaded.
 */
export function BucketRulesTab() {
  const { t, i18n } = useTranslation();
  const { bucket = '' } = useParams();
  const { accessRules, objects } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions({ bucket });
  const canEdit = perms.can(StorageActions.manageAccessRules);
  const [view, setView] = useState<View>('rules');
  const [editing, setEditing] = useState<Editing | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [pendingOrder, setPendingOrder] = useState<BucketAccessRule[] | null>(null);
  const [sent, setSent] = useState<Sent | null>(null);
  const [highlight, setHighlight] = useState<AccessRulesKeyResult | null>(null);

  const key = accessKeys.rules(bucket);
  const query = useQuery({ queryKey: key, queryFn: () => accessRules.get(bucket) });
  const status = useQuery({
    queryKey: accessKeys.publicStatus(bucket),
    queryFn: () => accessRules.publicStatus(bucket),
    enabled: query.isSuccess,
  });
  const samples = useQuery({
    queryKey: accessKeys.sampleKeys(bucket),
    queryFn: async () => {
      const page = await objects.list(bucket, { delimiter: '', limit: 12 });
      return page.objects.map((o) => o.key);
    },
    enabled: editing !== null,
    staleTime: 30_000,
  });

  const stored = query.data;
  const ordered = useMemo(() => orderedRules(stored?.rules ?? []), [stored]);
  const shown = pendingOrder ?? ordered;

  const refresh = async (next: AccessRules) => {
    queryClient.setQueryData(key, next);
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: accessKeys.publicStatus(bucket) }),
      queryClient.invalidateQueries({ queryKey: storageKeys.bucket(bucket), exact: true }),
      queryClient.invalidateQueries({ queryKey: storageKeys.buckets, exact: true }),
    ]);
  };

  const save = useMutation({
    mutationFn: (input: Sent) => {
      setSent(input);
      return accessRules.set(bucket, input.rules, stored?.etag);
    },
    onSuccess: async (next) => {
      await refresh(next);
      setPendingOrder(null);
      setEditing(null);
      setDeleting(null);
      setSent(null);
      notifications.show({ color: 'green', message: t('accessRules.saved') });
    },
    onError: () => {
      setPendingOrder(null);
    },
  });
  const preset = useMutation({
    mutationFn: (input: AccessPresetInput) => accessRules.applyPreset(bucket, input, stored?.etag),
    onSuccess: async (next, input) => {
      await refresh(next);
      notifications.show({
        color: 'green',
        message: t('presets.applied', { preset: t(`presets.names.${input.preset}`) }),
      });
    },
  });

  const conflict =
    (isApiError(save.error) && save.error.isPreconditionFailed) ||
    (isApiError(preset.error) && preset.error.isPreconditionFailed);

  const reload = () => {
    save.reset();
    preset.reset();
    setPendingOrder(null);
    void query.refetch();
  };

  const saveRules = (rules: BucketAccessRule[], edited: string | null = null) => {
    save.mutate({ rules, edited });
  };

  /** The stored rules with the edited one replaced (or added). */
  const withRule = (rule: BucketAccessRule, id: string | null): BucketAccessRule[] => {
    const rules = stored?.rules ?? [];
    return id === null ? [...rules, rule] : rules.map((r) => (r.id === id ? rule : r));
  };

  // The located problems of the rule in the editor (its index in what was sent).
  const sentIndex =
    sent?.edited !== undefined && sent.edited !== null && editing !== null
      ? sent.rules.findIndex((r) => r.id === editing.rule.id || r.id === sent.edited)
      : -1;
  const problems = problemsOf(save.error);
  const editorProblems =
    editing !== null ? problems.filter((p) => p.rule === sentIndex + 1 || p.rule === 0) : [];
  const editorError =
    editing !== null && save.isError && problems.length === 0 && !conflict
      ? problemMessage(t, save.error)
      : undefined;

  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {isApiError(query.error) && query.error.isForbidden
          ? t('accessRules.unavailable')
          : problemMessage(t, query.error)}
      </Alert>
    );
  }

  const locale = i18n.resolvedLanguage;

  return (
    <Stack gap="md">
      <Paper withBorder radius="md" p="md" component="section" aria-label={t('accessRules.title')}>
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
            <Stack gap={2}>
              <Group gap="xs">
                <Title order={4}>{t('accessRules.title')}</Title>
                {stored !== undefined ? (
                  <Badge
                    color={stored.public ? 'orange' : 'gray'}
                    variant="light"
                    leftSection={<IconWorld size={12} />}
                  >
                    {stored.public ? t('buckets.public') : t('accessRules.private')}
                  </Badge>
                ) : null}
                {stored?.blockPublic === true ? (
                  <Badge color="grape" variant="light">
                    {t('blockPublic.active')}
                  </Badge>
                ) : null}
              </Group>
              <Text size="sm" c="dimmed">
                {t('accessRules.description')}
              </Text>
            </Stack>
            <PermissionGuard allowed={canEdit} action={StorageActions.manageAccessRules}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled || query.isPending}
                  onClick={() => {
                    save.reset();
                    setEditing({ id: null, rule: newRule(stored?.rules ?? []) });
                  }}
                >
                  {t('accessRules.add')}
                </Button>
              )}
            </PermissionGuard>
          </Group>

          {conflict ? (
            <Alert
              color="yellow"
              variant="light"
              role="alert"
              icon={<IconAlertTriangle size={18} />}
              title={t('accessRules.conflictTitle')}
            >
              <Stack gap="xs" align="flex-start">
                <Text size="sm">{t('accessRules.conflict')}</Text>
                <Button
                  size="xs"
                  variant="default"
                  leftSection={<IconRefresh size={14} />}
                  onClick={reload}
                >
                  {t('accessRules.reload')}
                </Button>
              </Stack>
            </Alert>
          ) : null}
          {(stored?.warnings.length ?? 0) > 0 ? (
            <Alert color="orange" variant="light" icon={<IconAlertTriangle size={18} />}>
              <List size="sm" spacing={2}>
                {stored?.warnings.map((w) => (
                  <List.Item key={w}>{w}</List.Item>
                ))}
              </List>
            </Alert>
          ) : null}
          {editing === null && !conflict && save.isError ? (
            <Alert color="red" variant="light" role="alert" title={t('accessRules.serverRefused')}>
              {problems.length > 0 ? (
                <Stack gap={2}>
                  {problems.map((p) => (
                    <Text key={`${String(p.rule)}-${p.field ?? ''}-${p.message}`} size="sm">
                      {p.rule > 0
                        ? t('accessRules.ruleProblem', {
                            n: p.rule,
                            id: p.id ?? '',
                            message:
                              p.position !== undefined
                                ? t('pattern.errorAt', { position: p.position, message: p.message })
                                : p.message,
                          })
                        : p.message}
                    </Text>
                  ))}
                </Stack>
              ) : (
                problemMessage(t, save.error)
              )}
            </Alert>
          ) : null}

          <SegmentedControl
            aria-label={t('accessRules.viewAs')}
            value={view}
            onChange={(next) => {
              save.reset();
              setView(next as View);
            }}
            data={[
              { value: 'rules', label: t('accessRules.views.rules') },
              { value: 'yaml', label: 'YAML' },
              { value: 'json', label: 'JSON' },
            ]}
            style={{ alignSelf: 'flex-start' }}
          />

          {query.isPending ? (
            <Skeleton height={120} />
          ) : view === 'rules' ? (
            shown.length === 0 ? (
              <Text size="sm" c="dimmed">
                {t('accessRules.empty')}
              </Text>
            ) : (
              <Stack gap="xs">
                {canEdit ? (
                  <Text size="xs" c="dimmed">
                    {t('accessRules.reorderHint')}
                  </Text>
                ) : null}
                <RulesTable
                  rules={shown}
                  canEdit={canEdit}
                  busy={save.isPending}
                  highlight={
                    highlight === null
                      ? undefined
                      : { matched: new Set(highlight.matched), determinant: highlight.determinant }
                  }
                  onMove={(from, to) => {
                    const next = moveRule(shown, from, to);
                    setPendingOrder(next);
                    saveRules(next);
                  }}
                  onEdit={(index) => {
                    const rule = shown[index];
                    if (rule === undefined) return;
                    save.reset();
                    setEditing({ id: rule.id, rule });
                  }}
                  onDelete={(index) => {
                    save.reset();
                    setDeleting(shown[index]?.id ?? null);
                  }}
                />
              </Stack>
            )
          ) : (
            <RulesCodeView
              key={`${view}-${stored?.etag ?? ''}`}
              format={view}
              bucket={bucket}
              rules={stored?.rules ?? []}
              canEdit={canEdit}
              saving={save.isPending}
              onSave={(rules) => {
                saveRules(rules);
              }}
            />
          )}
          {stored?.updatedAt !== undefined ? (
            <Text size="xs" c="dimmed">
              {t('accessRules.updated', {
                date: new Date(stored.updatedAt).toLocaleString(locale),
                by: stored.updatedBy ?? '',
              })}
            </Text>
          ) : null}
        </Stack>
      </Paper>

      <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
        <PresetPanel
          status={status.data}
          blocked={stored?.blockPublic === true}
          canEdit={canEdit}
          applying={preset.isPending}
          onApply={(input) => {
            preset.mutate(input);
          }}
        />
        <Stack gap="md">
          <BlockPublicControl bucket={bucket} canEdit={canEdit} />
          {preset.isError && !conflict ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, preset.error)}
            </Alert>
          ) : null}
        </Stack>
      </SimpleGrid>

      <RuleTester bucket={bucket} selected={highlight?.key ?? null} onSelect={setHighlight} />

      {editing !== null ? (
        <RuleEditor
          rule={editing.rule}
          isNew={editing.id === null}
          others={(stored?.rules ?? []).filter((r) => r.id !== editing.id)}
          sampleKeys={samples.data ?? []}
          saving={save.isPending}
          serverProblems={editorProblems}
          serverError={editorError}
          readOnly={!canEdit}
          onSave={(rule) => {
            saveRules(withRule(rule, editing.id), editing.id ?? rule.id);
          }}
          onClose={() => {
            setEditing(null);
            save.reset();
          }}
        />
      ) : null}
      <ConfirmDialog
        opened={deleting !== null}
        title={t('accessRules.deleteTitle')}
        message={t('accessRules.deleteConfirm', { id: deleting ?? '' })}
        confirmLabel={t('common.delete')}
        loading={save.isPending}
        error={deleting !== null && save.isError ? save.error : undefined}
        onCancel={() => {
          setDeleting(null);
          save.reset();
        }}
        onConfirm={() => {
          if (deleting !== null) saveRules((stored?.rules ?? []).filter((r) => r.id !== deleting));
        }}
      />
    </Stack>
  );
}
