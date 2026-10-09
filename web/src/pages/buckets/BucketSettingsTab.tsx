import {
  Alert,
  Autocomplete,
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
import { useKms } from '@/lib/kms';
import { useServices, type TagSet } from '@/services';

import { CorsSection } from './CorsSection';

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

type EncryptionChoice = 'none' | 'AES256' | 'aws:kms';

function EncryptionSection({
  bucket,
  canEdit,
  canSeeKms,
}: {
  bucket: string;
  canEdit: boolean;
  canSeeKms: boolean;
}) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const notifyError = useNotifyError();
  const kms = useKms(canSeeKms);
  const encryption = useQuery({
    queryKey: storageKeys.bucketSetting(bucket, 'encryption'),
    queryFn: () => buckets.getEncryption(bucket),
  });
  const [choice, setChoice] = useState<EncryptionChoice>('none');
  const [kmsKey, setKmsKey] = useState('');
  useEffect(() => {
    if (encryption.data === undefined) return;
    const algorithm = encryption.data?.algorithm;
    setChoice(algorithm === 'AES256' || algorithm === 'aws:kms' ? algorithm : 'none');
    setKmsKey(encryption.data?.kmsKeyId ?? '');
  }, [encryption.data]);
  const mutation = useMutation({
    mutationFn: () =>
      buckets.setEncryption(
        bucket,
        choice === 'none'
          ? null
          : {
              algorithm: choice,
              ...(choice === 'aws:kms' && kmsKey.trim() !== '' ? { kmsKeyId: kmsKey.trim() } : {}),
            },
      ),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: storageKeys.bucketSetting(bucket, 'encryption'),
      });
      notifications.show({ color: 'green', message: t('bucketSettings.saved') });
    },
    onError: notifyError,
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
        <form
          onSubmit={(event) => {
            event.preventDefault();
            mutation.mutate();
          }}
        >
          <Stack gap="xs">
            <Group gap="sm">
              <Text size="sm">{t('bucketSettings.encryption.current')}</Text>
              <Badge variant="light" color={encryption.data ? 'green' : 'gray'}>
                {encryption.data
                  ? encryption.data.algorithm === 'aws:kms'
                    ? t('bucketSettings.encryption.kms', { key: encryption.data.kmsKeyId ?? '' })
                    : t('bucketSettings.encryption.sse')
                  : t('bucketSettings.encryption.none')}
              </Badge>
            </Group>
            <Group gap="sm" align="flex-end" wrap="wrap">
              <Select
                label={t('bucketSettings.encryption.algorithm')}
                data={[
                  { value: 'none', label: t('bucketSettings.encryption.none') },
                  { value: 'AES256', label: t('bucketSettings.encryption.sse') },
                  { value: 'aws:kms', label: t('bucketSettings.encryption.kmsOption') },
                ]}
                value={choice}
                allowDeselect={false}
                disabled={!canEdit}
                onChange={(next) => {
                  if (next === 'none' || next === 'AES256' || next === 'aws:kms') setChoice(next);
                }}
                w={220}
              />
              {choice === 'aws:kms' ? (
                <Autocomplete
                  label={t('bucketSettings.encryption.kmsKey')}
                  description={t('bucketSettings.encryption.kmsKeyHint')}
                  placeholder={kms.defaultKeyId}
                  data={kms.keys}
                  value={kmsKey}
                  disabled={!canEdit}
                  onChange={setKmsKey}
                  w={260}
                />
              ) : null}
              <PermissionGuard allowed={canEdit} action={StorageActions.putEncryption}>
                {(disabled) => (
                  <Button type="submit" disabled={disabled} loading={mutation.isPending}>
                    {t('common.save')}
                  </Button>
                )}
              </PermissionGuard>
            </Group>
            <Text size="xs" c="dimmed">
              {t('bucketSettings.encryption.newObjects')}
            </Text>
            {kms.configured === false ? (
              <Alert color="yellow" variant="light">
                {t('bucketSettings.encryption.noKms')}
              </Alert>
            ) : null}
          </Stack>
        </form>
      )}
    </Section>
  );
}

type RetentionUnit = 'days' | 'years';

function ObjectLockSection({ bucket, canEdit }: { bucket: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const notifyError = useNotifyError();
  const lock = useQuery({
    queryKey: storageKeys.bucketSetting(bucket, 'objectlock'),
    queryFn: () => buckets.getObjectLock(bucket),
  });
  const [mode, setMode] = useState<'none' | 'GOVERNANCE' | 'COMPLIANCE'>('none');
  const [period, setPeriod] = useState<number | string>(30);
  const [unit, setUnit] = useState<RetentionUnit>('days');
  useEffect(() => {
    const data = lock.data;
    if (data === undefined) return;
    setMode(data.mode ?? 'none');
    if (data.years !== undefined && data.years > 0) {
      setPeriod(data.years);
      setUnit('years');
    } else if (data.days !== undefined && data.days > 0) {
      setPeriod(data.days);
      setUnit('days');
    }
  }, [lock.data]);
  const periodValue = typeof period === 'number' ? period : Number(period);
  const invalid = mode !== 'none' && (!Number.isInteger(periodValue) || periodValue < 1);
  const mutation = useMutation({
    mutationFn: () =>
      buckets.setObjectLock(bucket, {
        enabled: true,
        ...(mode === 'none' ? {} : { mode, [unit]: periodValue }),
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: storageKeys.bucketSetting(bucket, 'objectlock'),
      });
      await queryClient.invalidateQueries({ queryKey: storageKeys.bucket(bucket) });
      notifications.show({ color: 'green', message: t('bucketSettings.saved') });
    },
    onError: notifyError,
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
      ) : !data.enabled ? (
        <Group gap="sm">
          <Badge variant="light" color="gray">
            {t('bucketSettings.objectLock.disabled')}
          </Badge>
          <Text size="sm" c="dimmed">
            {t('bucketSettings.objectLock.onlyAtCreation')}
          </Text>
        </Group>
      ) : (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (!invalid) mutation.mutate();
          }}
        >
          <Stack gap="xs">
            <Group gap="sm">
              <Text size="sm">{t('bucketSettings.objectLock.status')}</Text>
              <Badge variant="light" color="green">
                {t('bucketSettings.objectLock.enabled')}
              </Badge>
              <Text size="sm" c="dimmed">
                {data.mode === undefined
                  ? t('bucketSettings.objectLock.noDefault')
                  : `${data.mode} · ${
                      data.years !== undefined && data.years > 0
                        ? t('bucketSettings.objectLock.years', { n: data.years })
                        : t('bucketSettings.objectLock.days', { n: data.days ?? 0 })
                    }`}
              </Text>
            </Group>
            <Group gap="sm" align="flex-end" wrap="wrap">
              <Select
                label={t('bucketSettings.objectLock.mode')}
                data={[
                  { value: 'none', label: t('bucketSettings.objectLock.noDefault') },
                  { value: 'GOVERNANCE', label: 'GOVERNANCE' },
                  { value: 'COMPLIANCE', label: 'COMPLIANCE' },
                ]}
                value={mode}
                allowDeselect={false}
                disabled={!canEdit}
                onChange={(next) => {
                  if (next === 'none' || next === 'GOVERNANCE' || next === 'COMPLIANCE') {
                    setMode(next);
                  }
                }}
                w={180}
              />
              {mode !== 'none' ? (
                <>
                  <NumberInput
                    label={t('bucketSettings.objectLock.retention')}
                    min={1}
                    allowDecimal={false}
                    allowNegative={false}
                    value={period}
                    onChange={setPeriod}
                    disabled={!canEdit}
                    error={invalid ? t('bucketSettings.objectLock.periodInvalid') : undefined}
                    w={140}
                  />
                  <Select
                    aria-label={t('bucketSettings.objectLock.unit')}
                    data={[
                      { value: 'days', label: t('bucketSettings.objectLock.unitDays') },
                      { value: 'years', label: t('bucketSettings.objectLock.unitYears') },
                    ]}
                    value={unit}
                    allowDeselect={false}
                    disabled={!canEdit}
                    onChange={(next) => {
                      if (next === 'days' || next === 'years') setUnit(next);
                    }}
                    w={120}
                  />
                </>
              ) : null}
              <PermissionGuard allowed={canEdit} action={StorageActions.putObjectLock}>
                {(disabled) => (
                  <Button type="submit" disabled={disabled || invalid} loading={mutation.isPending}>
                    {t('common.save')}
                  </Button>
                )}
              </PermissionGuard>
            </Group>
            <Text size="xs" c="dimmed">
              {t('bucketSettings.objectLock.permanent')}
            </Text>
          </Stack>
        </form>
      )}
    </Section>
  );
}

/** D-BKT-02: versioning, quota, tags, default encryption, object lock, CORS. */
export function BucketSettingsTab() {
  const { bucket = '' } = useParams();
  const perms = usePermissions({ bucket });
  return (
    <Stack gap="md">
      <VersioningSection bucket={bucket} canEdit={perms.can(StorageActions.putVersioning)} />
      <QuotaSection bucket={bucket} canEdit={perms.can(StorageActions.setQuota)} />
      <TagsSection bucket={bucket} canEdit={perms.can(StorageActions.putTagging)} />
      <EncryptionSection
        bucket={bucket}
        canEdit={perms.can(StorageActions.putEncryption)}
        canSeeKms={perms.can(StorageActions.kmsStatus)}
      />
      <ObjectLockSection bucket={bucket} canEdit={perms.can(StorageActions.putObjectLock)} />
      <CorsSection bucket={bucket} canEdit={perms.can(StorageActions.putCors)} />
    </Stack>
  );
}
