import { Alert, Button, Checkbox, Group, Modal, Skeleton, Stack, Text } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconLock } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, type PermissionCheck } from '@/auth/usePermissions';
import { deleteBlock } from '@/lib/objectLock';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type ObjectEntry } from '@/services';

/** The server kept the version: the reason, worded for the user. */
class RefusedDeletion extends Error {}

export interface VersionDeleteDialogProps {
  bucket: string;
  /** The version to delete; the dialog is closed when null. */
  version: ObjectEntry | null;
  perms: PermissionCheck;
  onClose: () => void;
  now?: () => Date;
}

/**
 * Permanent deletion of a version in a bucket with Object Lock: the
 * retention and legal hold of the version are shown first, with a clear
 * explanation when they forbid the deletion; a GOVERNANCE retention is
 * bypassed on request where the user has s3:BypassGovernanceRetention.
 */
export function VersionDeleteDialog({
  bucket,
  version,
  perms,
  onClose,
  now = () => new Date(),
}: VersionDeleteDialogProps) {
  const { t, i18n } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const [bypass, setBypass] = useState(false);
  const key = version?.key ?? '';
  const versionId = version?.versionId ?? 'null';
  const isMarker = version?.isDeleteMarker === true;
  const enabled = version !== null && !isMarker;
  const lockKey = storageKeys.objectLock(bucket, key, versionId);
  const retention = useQuery({
    queryKey: [...lockKey, 'retention'],
    queryFn: () => objects.getRetention(bucket, key, versionId),
    enabled: enabled && perms.can(StorageActions.getRetention),
  });
  const hold = useQuery({
    queryKey: [...lockKey, 'legal-hold'],
    queryFn: () => objects.getLegalHold(bucket, key, versionId),
    enabled: enabled && perms.can(StorageActions.getLegalHold),
  });
  const remove = useMutation({
    mutationFn: async () => {
      const result = await objects.remove(bucket, {
        objects: [{ key, versionId }],
        ...(bypass ? { bypassGovernance: true } : {}),
      });
      const error = result.errors[0];
      if (error !== undefined) {
        throw new RefusedDeletion(
          error.code === 'object_locked' ? t('objectLock.deleteRefused') : error.message,
        );
      }
      return result;
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.objects(bucket) });
      notifications.show({ color: 'green', message: t('objectVersions.deleted') });
      close();
    },
  });

  function close() {
    setBypass(false);
    remove.reset();
    onClose();
  }

  const block = deleteBlock(retention.data, hold.data, now());
  const canBypass = perms.can(StorageActions.bypassGovernance);
  const locale = i18n.resolvedLanguage;
  const until =
    retention.data?.retainUntil !== undefined
      ? new Date(retention.data.retainUntil).toLocaleString(locale)
      : '';
  const blocked =
    block === 'legalHold' || block === 'compliance' || (block === 'governance' && !bypass);
  const loading =
    (retention.isFetching && retention.isPending) || (hold.isFetching && hold.isPending);
  const refused = !remove.isError
    ? null
    : remove.error instanceof RefusedDeletion
      ? remove.error.message
      : problemMessage(t, remove.error);

  return (
    <Modal
      opened={version !== null}
      onClose={close}
      title={t('objectVersions.deleteTitle', { version: versionId })}
      centered
    >
      <Stack gap="md">
        <Text size="sm">
          {isMarker ? t('objectVersions.deleteMarkerIntro') : t('objectVersions.deleteIntro')}
        </Text>
        {loading ? <Skeleton height={40} /> : null}
        {block === 'legalHold' ? (
          <Alert color="red" variant="light" role="alert" icon={<IconLock size={18} />}>
            {t('objectLock.blockedLegalHold')}
          </Alert>
        ) : null}
        {block === 'compliance' ? (
          <Alert color="red" variant="light" role="alert" icon={<IconLock size={18} />}>
            {t('objectLock.blockedCompliance', { date: until })}
          </Alert>
        ) : null}
        {block === 'governance' ? (
          <Alert color="orange" variant="light" role="alert" icon={<IconLock size={18} />}>
            <Stack gap="xs">
              <Text size="sm">{t('objectLock.blockedGovernance', { date: until })}</Text>
              {canBypass ? (
                <Checkbox
                  label={t('objectLock.bypass')}
                  checked={bypass}
                  onChange={(event) => {
                    setBypass(event.currentTarget.checked);
                  }}
                />
              ) : (
                <Text size="sm">
                  {t('objectLock.bypassMissing', { action: StorageActions.bypassGovernance })}
                </Text>
              )}
            </Stack>
          </Alert>
        ) : null}
        {refused !== null ? (
          <Alert color="red" variant="light" role="alert">
            {refused}
          </Alert>
        ) : null}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={close}>
            {t('common.cancel')}
          </Button>
          <Button
            color="red"
            disabled={blocked || loading}
            loading={remove.isPending}
            onClick={() => {
              remove.mutate();
            }}
          >
            {t('objectVersions.deletePermanently')}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
