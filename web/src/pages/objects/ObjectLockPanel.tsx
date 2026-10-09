import '@mantine/dates/styles.css';

import {
  Alert,
  Badge,
  Button,
  Checkbox,
  Group,
  Paper,
  Select,
  Skeleton,
  Stack,
  Switch,
  Text,
  Title,
} from '@mantine/core';
import { DateTimePicker } from '@mantine/dates';
import { notifications } from '@mantine/notifications';
import { IconLock } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, type PermissionCheck } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { retentionChange } from '@/lib/objectLock';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type ObjectRetention, type RetentionMode } from '@/services';

type ModeChoice = 'none' | RetentionMode;

const day = 24 * 3600 * 1000;

/** Quick retain-until dates, from now. */
const presets = [
  { key: 'day', ms: day },
  { key: 'week', ms: 7 * day },
  { key: 'month', ms: 30 * day },
  { key: 'year', ms: 365 * day },
] as const;

export interface ObjectLockPanelProps {
  bucket: string;
  objectKey: string;
  /** The version shown (the latest when undefined). */
  versionId?: string | undefined;
  perms: PermissionCheck;
  /** Clock of the checks (tests). */
  now?: () => Date;
}

/** Retention of a version: mode and retain-until date, with the governance bypass. */
function RetentionEditor({
  bucket,
  objectKey,
  versionId,
  perms,
  now,
  current,
}: Required<Pick<ObjectLockPanelProps, 'bucket' | 'objectKey' | 'perms' | 'now'>> & {
  versionId: string | undefined;
  current: ObjectRetention;
}) {
  const { t, i18n } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<ModeChoice>('none');
  const [until, setUntil] = useState<Date | null>(null);
  const [bypass, setBypass] = useState(false);
  useEffect(() => {
    setMode(current.mode ?? 'none');
    setUntil(current.retainUntil !== undefined ? new Date(current.retainUntil) : null);
    setBypass(false);
  }, [current]);

  const canEdit = perms.can(StorageActions.putRetention);
  const canBypass = perms.can(StorageActions.bypassGovernance);
  const change = retentionChange(current, mode === 'none' ? undefined : mode, until, now());
  const save = useMutation({
    mutationFn: () =>
      objects.setRetention(
        bucket,
        objectKey,
        mode === 'none' || until === null
          ? { ...(change.needsBypass && bypass ? { bypassGovernance: true } : {}) }
          : {
              mode,
              retainUntil: until.toISOString(),
              ...(change.needsBypass && bypass ? { bypassGovernance: true } : {}),
            },
        versionId,
      ),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: storageKeys.objectLock(bucket, objectKey, versionId),
      });
      notifications.show({ color: 'green', message: t('objectLock.retentionSaved') });
    },
  });
  const locale = i18n.resolvedLanguage;
  const blocked =
    change.kind === 'invalid' ||
    change.kind === 'compliance' ||
    (change.needsBypass && (!canBypass || !bypass));

  return (
    <form
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (!blocked && change.kind !== 'unchanged') save.mutate();
      }}
    >
      <Stack gap="sm">
        <Group gap="xs">
          <Text size="sm">{t('objectLock.currentRetention')}</Text>
          {current.mode !== undefined && current.retainUntil !== undefined ? (
            <Badge
              variant="light"
              color={current.mode === 'COMPLIANCE' ? 'red' : 'orange'}
              leftSection={<IconLock size={12} />}
            >
              {t('objectLock.retainedUntil', {
                mode: current.mode,
                date: new Date(current.retainUntil).toLocaleString(locale),
              })}
            </Badge>
          ) : (
            <Badge variant="light" color="gray">
              {t('objectLock.noRetention')}
            </Badge>
          )}
        </Group>
        <Group align="flex-end" gap="sm" wrap="wrap">
          <Select
            label={t('objectLock.mode')}
            value={mode}
            allowDeselect={false}
            disabled={!canEdit}
            data={[
              { value: 'none', label: t('objectLock.modeNone') },
              { value: 'GOVERNANCE', label: 'GOVERNANCE' },
              { value: 'COMPLIANCE', label: 'COMPLIANCE' },
            ]}
            onChange={(next) => {
              if (next === 'none' || next === 'GOVERNANCE' || next === 'COMPLIANCE') {
                setMode(next);
                if (next !== 'none' && until === null)
                  setUntil(new Date(now().getTime() + 30 * day));
              }
            }}
            w={180}
          />
          {mode !== 'none' ? (
            <DateTimePicker
              label={t('objectLock.retainUntil')}
              value={until}
              minDate={now()}
              disabled={!canEdit}
              onChange={setUntil}
              error={change.kind === 'invalid' ? t('objectLock.dateInPast') : undefined}
              w={240}
            />
          ) : null}
        </Group>
        {mode !== 'none' && canEdit ? (
          <Group gap={4}>
            <Text size="xs" c="dimmed">
              {t('objectLock.quick')}
            </Text>
            {presets.map((p) => (
              <Button
                key={p.key}
                size="compact-xs"
                variant="light"
                onClick={() => {
                  setUntil(new Date(now().getTime() + p.ms));
                }}
              >
                {t(`objectLock.presets.${p.key}`)}
              </Button>
            ))}
          </Group>
        ) : null}
        {change.kind === 'compliance' ? (
          <Alert color="red" variant="light" role="alert">
            {t('objectLock.complianceLocked')}
          </Alert>
        ) : null}
        {change.needsBypass ? (
          canBypass ? (
            <Checkbox
              label={t('objectLock.bypass')}
              description={t('objectLock.bypassHint')}
              checked={bypass}
              onChange={(event) => {
                setBypass(event.currentTarget.checked);
              }}
            />
          ) : (
            <Alert color="yellow" variant="light" role="alert">
              {t('objectLock.bypassMissing', { action: StorageActions.bypassGovernance })}
            </Alert>
          )
        ) : null}
        {save.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, save.error)}
          </Alert>
        ) : null}
        <Group justify="flex-end">
          <PermissionGuard allowed={canEdit} action={StorageActions.putRetention}>
            {(disabled) => (
              <Button
                type="submit"
                disabled={disabled || blocked || change.kind === 'unchanged'}
                loading={save.isPending}
              >
                {t('objectLock.saveRetention')}
              </Button>
            )}
          </PermissionGuard>
        </Group>
      </Stack>
    </form>
  );
}

/**
 * D-OBJ-06: Object Lock of a version: its retention (mode and retain-until
 * date; shortening a GOVERNANCE retention needs the bypass, a COMPLIANCE
 * one never shortens) and its legal hold.
 */
export function ObjectLockPanel({
  bucket,
  objectKey,
  versionId,
  perms,
  now = () => new Date(),
}: ObjectLockPanelProps) {
  const { t } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const key = storageKeys.objectLock(bucket, objectKey, versionId);
  const canReadRetention = perms.can(StorageActions.getRetention);
  const canReadHold = perms.can(StorageActions.getLegalHold);
  const retention = useQuery({
    queryKey: [...key, 'retention'],
    queryFn: () => objects.getRetention(bucket, objectKey, versionId),
    enabled: canReadRetention,
  });
  const hold = useQuery({
    queryKey: [...key, 'legal-hold'],
    queryFn: () => objects.getLegalHold(bucket, objectKey, versionId),
    enabled: canReadHold,
  });
  const setHold = useMutation({
    mutationFn: (on: boolean) =>
      objects.setLegalHold(bucket, objectKey, on ? 'ON' : 'OFF', versionId),
    onSuccess: async (status) => {
      await queryClient.invalidateQueries({ queryKey: key });
      notifications.show({
        color: 'green',
        message: status === 'ON' ? t('objectLock.holdOn') : t('objectLock.holdOff'),
      });
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });
  const canHold = perms.can(StorageActions.putLegalHold);

  return (
    <Stack gap="md">
      <Paper
        withBorder
        radius="md"
        p="sm"
        component="section"
        aria-label={t('objectLock.retention')}
      >
        <Stack gap="xs">
          <Title order={5}>{t('objectLock.retention')}</Title>
          {!canReadRetention ? (
            <Text size="sm" c="dimmed">
              {t('permissions.missing', { action: StorageActions.getRetention })}
            </Text>
          ) : retention.isPending ? (
            <Skeleton height={60} />
          ) : retention.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, retention.error)}
            </Alert>
          ) : (
            <RetentionEditor
              bucket={bucket}
              objectKey={objectKey}
              versionId={versionId}
              perms={perms}
              now={now}
              current={retention.data}
            />
          )}
        </Stack>
      </Paper>
      <Paper
        withBorder
        radius="md"
        p="sm"
        component="section"
        aria-label={t('objectLock.legalHold')}
      >
        <Stack gap="xs">
          <Title order={5}>{t('objectLock.legalHold')}</Title>
          <Text size="xs" c="dimmed">
            {t('objectLock.legalHoldHint')}
          </Text>
          {!canReadHold ? (
            <Text size="sm" c="dimmed">
              {t('permissions.missing', { action: StorageActions.getLegalHold })}
            </Text>
          ) : hold.isPending ? (
            <Skeleton height={24} width={160} />
          ) : hold.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, hold.error)}
            </Alert>
          ) : (
            <PermissionGuard allowed={canHold} action={StorageActions.putLegalHold}>
              {(disabled) => (
                <Switch
                  label={t('objectLock.legalHoldSwitch')}
                  checked={hold.data === 'ON'}
                  disabled={disabled || setHold.isPending}
                  onChange={(event) => {
                    setHold.mutate(event.currentTarget.checked);
                  }}
                />
              )}
            </PermissionGuard>
          )}
        </Stack>
      </Paper>
    </Stack>
  );
}
