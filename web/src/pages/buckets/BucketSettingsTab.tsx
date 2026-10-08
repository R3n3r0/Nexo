import {
  Badge,
  Button,
  Group,
  NumberInput,
  Paper,
  Progress,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  Table,
  Text,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { TagEditor } from '@/components/storage/TagEditor';
import { formatBytes } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { fromBytes, sizeUnits, toBytes, type SizeUnit } from '@/lib/units';
import { useServices, type TagSet } from '@/services';

function Section({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children: ReactNode;
}) {
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Stack gap={2}>
          <Title order={4}>{title}</Title>
          {description !== undefined ? (
            <Text size="sm" c="dimmed">
              {description}
            </Text>
          ) : null}
        </Stack>
        {children}
      </Stack>
    </Paper>
  );
}

function useNotifyError() {
  const { t } = useTranslation();
  return (error: unknown) => {
    notifications.show({
      color: 'red',
      title: t('common.error'),
      message: problemMessage(t, error),
    });
  };
}

function VersioningSection({ bucket, canEdit }: { bucket: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const notifyError = useNotifyError();
  const details = useQuery({
    queryKey: storageKeys.bucket(bucket),
    queryFn: () => buckets.get(bucket),
  });
  const mutation = useMutation({
    mutationFn: (status: 'enabled' | 'suspended') => buckets.setVersioning(bucket, status),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.bucket(bucket) });
      await queryClient.invalidateQueries({ queryKey: storageKeys.buckets });
      notifications.show({ color: 'green', message: t('bucketSettings.saved') });
    },
    onError: notifyError,
  });
  const state = details.data?.versioning;
  const locked = details.data?.objectLockEnabled === true;

  return (
    <Section
      title={t('bucketSettings.versioning.title')}
      description={t('bucketSettings.versioning.description')}
    >
      {state === undefined ? (
        <Skeleton height={32} width={240} />
      ) : (
        <Stack gap="xs">
          <Group gap="sm">
            <Text size="sm">{t('bucketSettings.versioning.current')}</Text>
            <Badge variant="light" color={state === 'enabled' ? 'green' : 'gray'}>
              {t(`bucketSettings.versioning.states.${state}`)}
            </Badge>
          </Group>
          <PermissionGuard allowed={canEdit} action={StorageActions.putVersioning}>
            {(disabled) => (
              <SegmentedControl
                aria-label={t('bucketSettings.versioning.title')}
                value={state === 'enabled' ? 'enabled' : 'suspended'}
                disabled={disabled || mutation.isPending || locked}
                data={[
                  { value: 'enabled', label: t('bucketSettings.versioning.enable') },
                  { value: 'suspended', label: t('bucketSettings.versioning.suspend') },
                ]}
                onChange={(value) => {
                  if (value === 'enabled' || value === 'suspended') {
                    mutation.mutate(value);
                  }
                }}
              />
            )}
          </PermissionGuard>
          {locked ? (
            <Text size="xs" c="dimmed">
              {t('bucketSettings.versioning.lockedHint')}
            </Text>
          ) : null}
        </Stack>
      )}
    </Section>
  );
}

function QuotaSection({ bucket, canEdit }: { bucket: string; canEdit: boolean }) {
  const { t, i18n } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const notifyError = useNotifyError();
  const details = useQuery({
    queryKey: storageKeys.bucket(bucket),
    queryFn: () => buckets.get(bucket),
  });
  const [value, setValue] = useState<number | string>('');
  const [unit, setUnit] = useState<SizeUnit>('GiB');
  const quotaBytes = details.data?.quotaBytes;

  useEffect(() => {
    if (quotaBytes === undefined) return;
    if (quotaBytes > 0) {
      const parsed = fromBytes(quotaBytes);
      setValue(parsed.value);
      setUnit(parsed.unit);
    } else {
      setValue('');
    }
  }, [quotaBytes]);

  const mutation = useMutation({
    mutationFn: (bytes: number) => buckets.setQuota(bucket, bytes),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.bucket(bucket) });
      await queryClient.invalidateQueries({ queryKey: storageKeys.buckets });
      notifications.show({ color: 'green', message: t('bucketSettings.saved') });
    },
    onError: notifyError,
  });
  const numeric = typeof value === 'number' ? value : value === '' ? 0 : Number.NaN;
  const invalid = !Number.isFinite(numeric) || numeric < 0;
  const used = details.data?.size ?? 0;
  const locale = i18n.resolvedLanguage;

  return (
    <Section
      title={t('bucketSettings.quota.title')}
      description={t('bucketSettings.quota.description')}
    >
      {quotaBytes !== undefined && quotaBytes > 0 ? (
        <Stack gap={4}>
          <Progress
            value={Math.min(100, (used / quotaBytes) * 100)}
            color={used >= quotaBytes ? 'red' : used / quotaBytes > 0.8 ? 'yellow' : 'blue'}
            aria-label={t('bucketSettings.quota.usage')}
          />
          <Text size="xs" c="dimmed">
            {t('bucketSettings.quota.usedOf', {
              used: formatBytes(used, locale),
              total: formatBytes(quotaBytes, locale),
            })}
          </Text>
        </Stack>
      ) : null}
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (!invalid) {
            mutation.mutate(numeric > 0 ? toBytes(numeric, unit) : 0);
          }
        }}
      >
        <Group align="flex-end" gap="xs" wrap="wrap">
          <NumberInput
            label={t('bucketSettings.quota.limit')}
            description={t('bucketSettings.quota.hint')}
            min={0}
            decimalScale={2}
            value={value}
            onChange={setValue}
            disabled={!canEdit}
            error={invalid ? t('buckets.quotaInvalid') : undefined}
            w={200}
          />
          <Select
            aria-label={t('buckets.quotaUnit')}
            data={[...sizeUnits]}
            value={unit}
            allowDeselect={false}
            disabled={!canEdit}
            onChange={(next) => {
              if (next !== null) setUnit(next as SizeUnit);
            }}
            w={100}
          />
          <PermissionGuard allowed={canEdit} action={StorageActions.setQuota}>
            {(disabled) => (
              <Button type="submit" disabled={disabled || invalid} loading={mutation.isPending}>
                {t('common.save')}
              </Button>
            )}
          </PermissionGuard>
        </Group>
      </form>
    </Section>
  );
}

function TagsSection({ bucket, canEdit }: { bucket: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const notifyError = useNotifyError();
  const tags = useQuery({
    queryKey: storageKeys.bucketSetting(bucket, 'tags'),
    queryFn: () => buckets.getTags(bucket),
  });
  const mutation = useMutation({
    mutationFn: (next: TagSet) => buckets.setTags(bucket, next),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.bucket(bucket) });
      notifications.show({ color: 'green', message: t('bucketSettings.saved') });
    },
    onError: notifyError,
  });
  return (
    <Section
      title={t('bucketSettings.tags.title')}
      description={t('bucketSettings.tags.description')}
    >
      {tags.isPending ? (
        <Skeleton height={60} />
      ) : (
        <TagEditor
          tags={tags.data}
          canEdit={canEdit}
          action={StorageActions.putTagging}
          saving={mutation.isPending}
          onSave={(next) => {
            mutation.mutate(next);
          }}
        />
      )}
    </Section>
  );
}

function EncryptionSection({ bucket }: { bucket: string }) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const encryption = useQuery({
    queryKey: storageKeys.bucketSetting(bucket, 'encryption'),
    queryFn: () => buckets.getEncryption(bucket),
  });
  return (
    <Section
      title={t('bucketSettings.encryption.title')}
      description={t('bucketSettings.encryption.description')}
    >
      {encryption.isPending ? (
        <Skeleton height={20} width={200} />
      ) : encryption.isError ? (
        <Text size="sm" c="dimmed">
          {t('bucketSettings.encryption.unavailable')}
        </Text>
      ) : (
        <Group gap="sm">
          <Badge variant="light" color={encryption.data ? 'green' : 'gray'}>
            {encryption.data
              ? encryption.data.algorithm === 'aws:kms'
                ? t('bucketSettings.encryption.kms', { key: encryption.data.kmsKeyId ?? '' })
                : t('bucketSettings.encryption.sse')
              : t('bucketSettings.encryption.none')}
          </Badge>
        </Group>
      )}
      <Text size="xs" c="dimmed">
        {t('bucketSettings.encryption.comingSoon')}
      </Text>
    </Section>
  );
}

function ObjectLockSection({ bucket }: { bucket: string }) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const lock = useQuery({
    queryKey: storageKeys.bucketSetting(bucket, 'objectlock'),
    queryFn: () => buckets.getObjectLock(bucket),
  });
  const data = lock.data;
  return (
    <Section
      title={t('bucketSettings.objectLock.title')}
      description={t('bucketSettings.objectLock.description')}
    >
      {lock.isPending ? (
        <Skeleton height={20} width={200} />
      ) : lock.isError || !data ? (
        <Text size="sm" c="dimmed">
          {t('bucketSettings.objectLock.unavailable')}
        </Text>
      ) : (
        <Table variant="vertical" layout="fixed" withTableBorder={false} maw={480}>
          <Table.Tbody>
            <Table.Tr>
              <Table.Th w={180}>{t('bucketSettings.objectLock.status')}</Table.Th>
              <Table.Td>
                <Badge variant="light" color={data.enabled ? 'green' : 'gray'}>
                  {data.enabled
                    ? t('bucketSettings.objectLock.enabled')
                    : t('bucketSettings.objectLock.disabled')}
                </Badge>
              </Table.Td>
            </Table.Tr>
            {data.enabled ? (
              <>
                <Table.Tr>
                  <Table.Th>{t('bucketSettings.objectLock.mode')}</Table.Th>
                  <Table.Td>{data.mode ?? t('bucketSettings.objectLock.noDefault')}</Table.Td>
                </Table.Tr>
                <Table.Tr>
                  <Table.Th>{t('bucketSettings.objectLock.retention')}</Table.Th>
                  <Table.Td>
                    {data.days !== undefined && data.days > 0
                      ? t('bucketSettings.objectLock.days', { n: data.days })
                      : data.years !== undefined && data.years > 0
                        ? t('bucketSettings.objectLock.years', { n: data.years })
                        : t('bucketSettings.objectLock.noDefault')}
                  </Table.Td>
                </Table.Tr>
              </>
            ) : null}
          </Table.Tbody>
        </Table>
      )}
    </Section>
  );
}

/** D-BKT-02: versioning, quota, tags, encryption (placeholder), object lock (read-only). */
export function BucketSettingsTab() {
  const { bucket = '' } = useParams();
  const perms = usePermissions({ bucket });
  return (
    <Stack gap="md">
      <VersioningSection bucket={bucket} canEdit={perms.can(StorageActions.putVersioning)} />
      <QuotaSection bucket={bucket} canEdit={perms.can(StorageActions.setQuota)} />
      <TagsSection bucket={bucket} canEdit={perms.can(StorageActions.putTagging)} />
      <EncryptionSection bucket={bucket} />
      <ObjectLockSection bucket={bucket} />
    </Stack>
  );
}
