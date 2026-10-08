import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Paper,
  Select,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  VisuallyHidden,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconTrash, IconWorld } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { CodeEditor } from '@/components/code/CodeEditor';
import type { CodeMarker } from '@/components/code/types';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices, type AccessPreset, type SetAccessInput } from '@/services';

const presets = ['private', 'download', 'upload', 'public'] as const;
type SettablePreset = (typeof presets)[number];

const presetColor: Record<AccessPreset, string> = {
  private: 'gray',
  download: 'blue',
  upload: 'grape',
  public: 'orange',
  custom: 'yellow',
};

/** Policy skeleton offered when the bucket has no policy yet. */
// eslint-disable-next-line react-refresh/only-export-components -- constant used by tests
export function policyTemplate(bucket: string): string {
  return JSON.stringify(
    {
      Version: '2012-10-17',
      Statement: [
        {
          Sid: 'PublicRead',
          Effect: 'Allow',
          Principal: { AWS: ['*'] },
          Action: ['s3:GetObject'],
          Resource: [`arn:aws:s3:::${bucket}/public/*`],
        },
      ],
    },
    null,
    2,
  );
}

function invalidateAccess(queryClient: ReturnType<typeof useQueryClient>, bucket: string) {
  return Promise.all([
    queryClient.invalidateQueries({ queryKey: storageKeys.bucketSetting(bucket, 'access') }),
    queryClient.invalidateQueries({ queryKey: storageKeys.bucketSetting(bucket, 'policy') }),
    queryClient.invalidateQueries({ queryKey: storageKeys.bucket(bucket) }),
    queryClient.invalidateQueries({ queryKey: storageKeys.buckets }),
  ]);
}

function AnonymousAccess({ bucket, canEdit }: { bucket: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const [prefix, setPrefix] = useState('');
  const [preset, setPreset] = useState<SettablePreset>('download');
  const access = useQuery({
    queryKey: storageKeys.bucketSetting(bucket, 'access'),
    queryFn: () => buckets.getAccess(bucket),
  });
  const mutation = useMutation({
    mutationFn: (input: SetAccessInput) => buckets.setAccess(bucket, input),
    onSuccess: async (_, input) => {
      await invalidateAccess(queryClient, bucket);
      notifications.show({
        color: 'green',
        message: t('bucketAccess.applied', {
          preset: t(`bucketAccess.presets.${input.preset}`),
          prefix:
            input.prefix === undefined || input.prefix === ''
              ? t('bucketAccess.wholeBucket')
              : input.prefix,
        }),
      });
      setPrefix('');
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });

  const rules = access.data?.rules ?? [];
  const prefixInvalid = prefix.startsWith('/');

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('bucketAccess.anonymousTitle')}
    >
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Stack gap={2}>
            <Title order={4}>{t('bucketAccess.anonymousTitle')}</Title>
            <Text size="sm" c="dimmed">
              {t('bucketAccess.anonymousDescription')}
            </Text>
          </Stack>
          {access.data ? (
            <Badge
              size="lg"
              variant="light"
              color={access.data.public ? 'orange' : 'gray'}
              leftSection={<IconWorld size={14} />}
            >
              {access.data.public ? t('bucketAccess.statePublic') : t('bucketAccess.statePrivate')}
            </Badge>
          ) : null}
        </Group>

        {access.isPending ? (
          <Skeleton height={80} />
        ) : access.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, access.error)}
          </Alert>
        ) : rules.length === 0 ? (
          <Text size="sm">{t('bucketAccess.noRules')}</Text>
        ) : (
          <Table aria-label={t('bucketAccess.rulesTitle')} verticalSpacing="xs">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('bucketAccess.prefix')}</Table.Th>
                <Table.Th>{t('bucketAccess.preset')}</Table.Th>
                <Table.Th w={48}>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rules.map((rule) => (
                <Table.Tr key={rule.prefix}>
                  <Table.Td>
                    {rule.prefix === '' ? (
                      <Text size="sm" fs="italic">
                        {t('bucketAccess.wholeBucket')}
                      </Text>
                    ) : (
                      <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                        {rule.prefix}
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <Badge variant="light" color={presetColor[rule.preset]}>
                      {t(`bucketAccess.presets.${rule.preset}`)}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    {rule.preset !== 'custom' ? (
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        disabled={!canEdit || mutation.isPending}
                        aria-label={t('bucketAccess.removeRule', {
                          prefix: rule.prefix === '' ? t('bucketAccess.wholeBucket') : rule.prefix,
                        })}
                        onClick={() => {
                          mutation.mutate({ prefix: rule.prefix, preset: 'private' });
                        }}
                      >
                        <IconTrash size={16} />
                      </ActionIcon>
                    ) : null}
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}

        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (!prefixInvalid) {
              mutation.mutate({ prefix, preset });
            }
          }}
        >
          <Group align="flex-end" gap="xs" wrap="wrap">
            <TextInput
              label={t('bucketAccess.prefix')}
              description={t('bucketAccess.prefixHint')}
              placeholder="public/"
              value={prefix}
              disabled={!canEdit}
              error={prefixInvalid ? t('bucketAccess.prefixInvalid') : undefined}
              onChange={(event) => {
                setPrefix(event.currentTarget.value);
              }}
              style={{ flex: '1 1 220px' }}
            />
            <Select
              label={t('bucketAccess.preset')}
              data={presets.map((p) => ({ value: p, label: t(`bucketAccess.presets.${p}`) }))}
              value={preset}
              allowDeselect={false}
              disabled={!canEdit}
              onChange={(value) => {
                if (value !== null) setPreset(value as SettablePreset);
              }}
              w={180}
            />
            <PermissionGuard allowed={canEdit} action={StorageActions.putBucketPolicy}>
              {(disabled) => (
                <Button
                  type="submit"
                  disabled={disabled || prefixInvalid}
                  loading={mutation.isPending}
                >
                  {t('bucketAccess.apply')}
                </Button>
              )}
            </PermissionGuard>
          </Group>
        </form>
        <Text size="xs" c="dimmed">
          {t(`bucketAccess.presetHelp.${preset}`)}
        </Text>
      </Stack>
    </Paper>
  );
}

function BucketPolicyEditor({
  bucket,
  canEdit,
  canDelete,
}: {
  bucket: string;
  canEdit: boolean;
  canDelete: boolean;
}) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const policy = useQuery({
    queryKey: storageKeys.bucketSetting(bucket, 'policy'),
    queryFn: () => buckets.getPolicy(bucket),
  });
  const [text, setText] = useState('');
  const [markers, setMarkers] = useState<CodeMarker[]>([]);
  const stored = policy.data;

  useEffect(() => {
    if (stored !== undefined) {
      setText(stored ?? '');
      setMarkers([]);
    }
  }, [stored]);

  const save = useMutation({
    mutationFn: (value: string) => buckets.setPolicy(bucket, value),
    onSuccess: async () => {
      setMarkers([]);
      await invalidateAccess(queryClient, bucket);
      notifications.show({ color: 'green', message: t('bucketAccess.policySaved') });
    },
    onError: (error) => {
      if (isApiError(error) && error.errors.length > 0) {
        setMarkers(
          error.errors.map((e) => ({ line: e.line, column: e.column, message: e.message })),
        );
        return;
      }
      setMarkers([]);
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });
  const remove = useMutation({
    mutationFn: () => buckets.deletePolicy(bucket),
    onSuccess: async () => {
      await invalidateAccess(queryClient, bucket);
      notifications.show({ color: 'green', message: t('bucketAccess.policyDeleted') });
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });

  const dirty = text !== (stored ?? '');
  const serverError =
    save.isError && !(isApiError(save.error) && save.error.errors.length > 0)
      ? problemMessage(t, save.error)
      : undefined;

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('bucketAccess.policyTitle')}
    >
      <Stack gap="sm">
        <Stack gap={2}>
          <Title order={4}>{t('bucketAccess.policyTitle')}</Title>
          <Text size="sm" c="dimmed">
            {t('bucketAccess.policyDescription')}
          </Text>
        </Stack>
        {policy.isPending ? (
          <Skeleton height={240} />
        ) : policy.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, policy.error)}
          </Alert>
        ) : (
          <>
            {stored === null && text === '' ? (
              <Group gap="sm">
                <Text size="sm">{t('bucketAccess.noPolicy')}</Text>
                {canEdit ? (
                  <Button
                    size="xs"
                    variant="default"
                    onClick={() => {
                      setText(policyTemplate(bucket));
                    }}
                  >
                    {t('bucketAccess.useTemplate')}
                  </Button>
                ) : null}
              </Group>
            ) : null}
            <CodeEditor
              label={t('bucketAccess.policyEditor')}
              value={text}
              onChange={(value) => {
                setText(value);
              }}
              readOnly={!canEdit}
              markers={markers}
              height={320}
              error={serverError}
            />
            <Group justify="flex-end" gap="xs">
              <PermissionGuard allowed={canDelete} action={StorageActions.deleteBucketPolicy}>
                {(disabled) => (
                  <Button
                    variant="default"
                    color="red"
                    disabled={disabled || stored === null}
                    loading={remove.isPending}
                    onClick={() => {
                      remove.mutate();
                    }}
                  >
                    {t('bucketAccess.deletePolicy')}
                  </Button>
                )}
              </PermissionGuard>
              <Button
                variant="default"
                disabled={!dirty}
                onClick={() => {
                  setText(stored ?? '');
                  setMarkers([]);
                }}
              >
                {t('bucketAccess.revert')}
              </Button>
              <PermissionGuard allowed={canEdit} action={StorageActions.putBucketPolicy}>
                {(disabled) => (
                  <Button
                    disabled={disabled || !dirty || text.trim() === ''}
                    loading={save.isPending}
                    onClick={() => {
                      save.mutate(text);
                    }}
                  >
                    {t('bucketAccess.savePolicy')}
                  </Button>
                )}
              </PermissionGuard>
            </Group>
          </>
        )}
      </Stack>
    </Paper>
  );
}

/** D-POL-01: anonymous presets per prefix and the bucket policy editor. */
export function BucketAccessTab() {
  const { bucket = '' } = useParams();
  const perms = usePermissions({ bucket });
  const canPut = perms.can(StorageActions.putBucketPolicy);
  return (
    <Stack gap="md">
      <AnonymousAccess bucket={bucket} canEdit={canPut} />
      <BucketPolicyEditor
        bucket={bucket}
        canEdit={canPut}
        canDelete={perms.can(StorageActions.deleteBucketPolicy)}
      />
    </Stack>
  );
}
