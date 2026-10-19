import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Checkbox,
  Group,
  Modal,
  MultiSelect,
  Paper,
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
import { IconEdit, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import {
  defaultMediaSuffixes,
  defaultThumbnailInterval,
  mediaRenditions,
  validateMediaRule,
  type MediaRuleIssue,
} from '@/lib/mediaRules';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import {
  isApiError,
  useServices,
  type MediaRendition,
  type MediaRule,
  type RuleProblem,
} from '@/services';

function ruleProblemsOf(error: unknown): readonly RuleProblem[] {
  return isApiError(error) ? error.ruleErrors : [];
}

interface RuleEditorProps {
  opened: boolean;
  rule: MediaRule | null;
  /** The ids of the other rules (duplicates). */
  otherIds: readonly string[];
  saving: boolean;
  problems: readonly RuleProblem[];
  error: string | undefined;
  onSave: (rule: MediaRule) => void;
  onClose: () => void;
}

/** Editor of one rule: id, keys selected, renditions, poster and thumbnails. */
function RuleEditor({
  opened,
  rule,
  otherIds,
  saving,
  problems,
  error,
  onSave,
  onClose,
}: RuleEditorProps) {
  const { t } = useTranslation();
  const [id, setId] = useState('');
  const [prefix, setPrefix] = useState('');
  const [suffixes, setSuffixes] = useState<string[]>([]);
  const [renditions, setRenditions] = useState<string[]>([]);
  const [poster, setPoster] = useState(false);
  const [thumbnails, setThumbnails] = useState(false);
  const [interval, setThumbInterval] = useState(defaultThumbnailInterval);
  const [submitted, setSubmitted] = useState(false);

  useEffect(() => {
    if (!opened) return;
    setId(rule?.id ?? '');
    setPrefix(rule?.prefix ?? '');
    setSuffixes(rule?.suffixes ?? []);
    setRenditions(rule?.renditions ?? (rule === null ? ['720p', '480p'] : []));
    setPoster(rule?.poster ?? rule === null);
    setThumbnails(rule?.thumbnails !== undefined);
    setThumbInterval(rule?.thumbnails?.interval ?? defaultThumbnailInterval);
    setSubmitted(false);
  }, [opened, rule]);

  const draft: MediaRule = {
    id: id.trim(),
    ...(prefix !== '' ? { prefix } : {}),
    ...(suffixes.length > 0 ? { suffixes } : {}),
    ...(renditions.length > 0 ? { renditions: renditions as MediaRendition[] } : {}),
    ...(poster ? { poster: true } : {}),
    ...(thumbnails ? { thumbnails: { interval: interval.trim() } } : {}),
  };
  const issues = validateMediaRule(draft, otherIds);
  const issue = (kind: readonly MediaRuleIssue[]) => {
    const found = submitted ? issues.find((i) => kind.includes(i)) : undefined;
    return found === undefined ? undefined : t(`bucketMedia.issues.${found}`);
  };
  const nothing = issue(['nothing']);

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={rule === null ? t('bucketMedia.addTitle') : t('bucketMedia.editTitle')}
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
          <TextInput
            label={t('bucketMedia.id')}
            description={t('bucketMedia.idHint')}
            placeholder="videos"
            required
            value={id}
            error={issue(['id', 'idDuplicate'])}
            onChange={(event) => {
              setId(event.currentTarget.value);
            }}
          />
          <TextInput
            label={t('bucketMedia.prefix')}
            description={t('bucketMedia.prefixHint')}
            placeholder="videos/"
            value={prefix}
            error={issue(['prefixTooLong', 'prefixReserved'])}
            onChange={(event) => {
              setPrefix(event.currentTarget.value);
            }}
          />
          <TagsInput
            label={t('bucketMedia.suffixes')}
            description={t('bucketMedia.suffixesHint', {
              suffixes: defaultMediaSuffixes.join(', '),
            })}
            placeholder={defaultMediaSuffixes.join(' ')}
            value={suffixes}
            error={issue(['suffix'])}
            onChange={setSuffixes}
          />
          <MultiSelect
            label={t('bucketMedia.renditions')}
            description={t('bucketMedia.renditionsHint')}
            data={mediaRenditions.map((r) => ({ value: r, label: r }))}
            value={renditions}
            onChange={setRenditions}
          />
          <Checkbox
            label={t('bucketMedia.poster')}
            description={t('bucketMedia.posterHint')}
            checked={poster}
            onChange={(event) => {
              setPoster(event.currentTarget.checked);
            }}
          />
          <Switch
            label={t('bucketMedia.thumbnails')}
            description={t('bucketMedia.thumbnailsHint')}
            checked={thumbnails}
            onChange={(event) => {
              setThumbnails(event.currentTarget.checked);
            }}
          />
          {thumbnails ? (
            <TextInput
              label={t('bucketMedia.interval')}
              description={t('bucketMedia.intervalHint')}
              placeholder={defaultThumbnailInterval}
              value={interval}
              error={issue(['interval'])}
              onChange={(event) => {
                setThumbInterval(event.currentTarget.value);
              }}
            />
          ) : null}
          {nothing !== undefined ? (
            <Text size="sm" c="red" role="alert">
              {nothing}
            </Text>
          ) : null}
          {problems.length > 0 || error !== undefined ? (
            <Alert color="red" variant="light" role="alert" title={t('bucketMedia.serverRefused')}>
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
              {t('bucketMedia.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/** What a rule derives from the videos it selects, as badges. */
function RuleOutputs({ rule }: { rule: MediaRule }) {
  const { t } = useTranslation();
  return (
    <Group gap={4}>
      {(rule.renditions ?? []).map((r) => (
        <Badge key={r} variant="outline" color="gray" tt="none">
          {r}
        </Badge>
      ))}
      {rule.poster === true ? (
        <Badge variant="light" tt="none">
          {t('bucketMedia.posterBadge')}
        </Badge>
      ) : null}
      {rule.thumbnails !== undefined ? (
        <Badge variant="light" color="grape" tt="none">
          {t('bucketMedia.thumbnailsBadge', {
            interval: rule.thumbnails.interval ?? defaultThumbnailInterval,
          })}
        </Badge>
      ) : null}
    </Group>
  );
}

/** The video rules of a bucket: what the media worker derives from its videos. */
export function BucketMediaTab() {
  const { t } = useTranslation();
  const { bucket = '' } = useParams();
  const { media } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions({ bucket });
  const canRead = !perms.ready || perms.can(StorageActions.getMedia);
  const canEdit = perms.can(StorageActions.setMedia);
  const [editing, setEditing] = useState<number | null>(null);
  const [deleting, setDeleting] = useState<number | null>(null);
  const key = storageKeys.bucketSetting(bucket, 'media');
  const query = useQuery({ queryKey: key, queryFn: () => media.get(bucket), enabled: canRead });
  const rules = query.data?.rules ?? [];

  const save = useMutation({
    mutationFn: async (next: MediaRule[]) => {
      if (next.length === 0) {
        await media.remove(bucket);
        return { rules: [] };
      }
      return media.set(bucket, { rules: next });
    },
    onSuccess: (stored) => {
      queryClient.setQueryData(key, stored);
      setEditing(null);
      setDeleting(null);
      notifications.show({ color: 'green', message: t('bucketMedia.saved') });
    },
  });
  const saveWith = (index: number, rule: MediaRule | null) => {
    save.mutate(
      rule === null
        ? rules.filter((_, i) => i !== index)
        : index < 0
          ? [...rules, rule]
          : rules.map((r, i) => (i === index ? rule : r)),
    );
  };
  const editedIndex = editing === null ? -1 : editing < 0 ? rules.length : editing;
  const problems = ruleProblemsOf(save.error).filter(
    (p) => p.rule === editedIndex + 1 || p.rule === 0,
  );

  if (!canRead || (query.isError && isApiError(query.error) && query.error.isForbidden)) {
    return (
      <Alert color="red" variant="light" role="alert">
        {t('bucketMedia.unavailable')}
      </Alert>
    );
  }
  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {problemMessage(t, query.error)}
      </Alert>
    );
  }

  return (
    <Stack gap="md">
      <Paper withBorder radius="md" p="md" component="section" aria-label={t('bucketMedia.title')}>
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
            <Stack gap={2}>
              <Title order={4}>{t('bucketMedia.title')}</Title>
              <Text size="sm" c="dimmed">
                {t('bucketMedia.description')}
              </Text>
            </Stack>
            <PermissionGuard allowed={canEdit} action={StorageActions.setMedia}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled || query.isPending}
                  onClick={() => {
                    save.reset();
                    setEditing(-1);
                  }}
                >
                  {t('bucketMedia.add')}
                </Button>
              )}
            </PermissionGuard>
          </Group>
          {query.isPending ? (
            <Skeleton height={100} />
          ) : rules.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t('bucketMedia.empty')}
            </Text>
          ) : (
            <Table.ScrollContainer minWidth={640}>
              <Table aria-label={t('bucketMedia.rules')} verticalSpacing="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('bucketMedia.columns.id')}</Table.Th>
                    <Table.Th>{t('bucketMedia.columns.objects')}</Table.Th>
                    <Table.Th>{t('bucketMedia.columns.outputs')}</Table.Th>
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
                        <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                          {rule.id}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Stack gap={0}>
                          <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                            {rule.prefix
                              ? t('bucketMedia.prefixIs', { prefix: rule.prefix })
                              : t('bucketMedia.wholeBucket')}
                          </Text>
                          <Text size="xs" c="dimmed">
                            {(rule.suffixes?.length ? rule.suffixes : defaultMediaSuffixes).join(
                              ', ',
                            )}
                          </Text>
                        </Stack>
                      </Table.Td>
                      <Table.Td>
                        <RuleOutputs rule={rule} />
                      </Table.Td>
                      <Table.Td>
                        {canEdit ? (
                          <Group gap={4} wrap="nowrap" justify="flex-end">
                            <Tooltip label={t('bucketMedia.edit', { id: rule.id })}>
                              <ActionIcon
                                variant="subtle"
                                aria-label={t('bucketMedia.edit', { id: rule.id })}
                                onClick={() => {
                                  save.reset();
                                  setEditing(index);
                                }}
                              >
                                <IconEdit size={16} />
                              </ActionIcon>
                            </Tooltip>
                            <Tooltip label={t('bucketMedia.remove', { id: rule.id })}>
                              <ActionIcon
                                variant="subtle"
                                color="red"
                                aria-label={t('bucketMedia.remove', { id: rule.id })}
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
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
          <Text size="xs" c="dimmed">
            {t('bucketMedia.firstMatch')}
          </Text>
        </Stack>
      </Paper>

      <RuleEditor
        opened={editing !== null}
        rule={editing === null || editing < 0 ? null : (rules[editing] ?? null)}
        otherIds={rules.filter((_, i) => i !== editing).map((r) => r.id)}
        saving={save.isPending}
        problems={editing !== null ? problems : []}
        error={
          editing !== null && save.isError && ruleProblemsOf(save.error).length === 0
            ? problemMessage(t, save.error)
            : undefined
        }
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
        title={t('bucketMedia.deleteTitle')}
        message={t('bucketMedia.deleteConfirm', {
          id: deleting === null ? '' : (rules[deleting]?.id ?? ''),
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
