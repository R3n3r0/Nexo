import { Alert, Button, Checkbox, Group, Modal, Stack, Text, TextInput } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { Trans, useTranslation } from 'react-i18next';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { formatNumber } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices, type Bucket } from '@/services';

export interface DeleteBucketDialogProps {
  /** The bucket to delete; the dialog is closed when null. */
  bucket: Bucket | null;
  onClose: () => void;
  onDeleted?: (name: string) => void;
}

/**
 * Deletion of a bucket, confirmed by typing its name. A non-empty bucket
 * needs "force" (s3:ForceDeleteBucket): the server empties it first.
 */
export function DeleteBucketDialog({ bucket, onClose, onDeleted }: DeleteBucketDialogProps) {
  const { t, i18n } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const [typed, setTyped] = useState('');
  const [force, setForce] = useState(false);
  const perms = usePermissions({ bucket: bucket?.name ?? '' }, { enabled: bucket !== null });
  const mutation = useMutation({
    mutationFn: (input: { name: string; force: boolean }) =>
      buckets.remove(input.name, { force: input.force }),
    onSuccess: async (_, input) => {
      queryClient.removeQueries({ queryKey: storageKeys.objects(input.name) });
      await queryClient.invalidateQueries({ queryKey: storageKeys.buckets });
      await queryClient.invalidateQueries({ queryKey: storageKeys.usage });
      notifications.show({ color: 'green', message: t('buckets.deleted', { name: input.name }) });
      onDeleted?.(input.name);
      close();
    },
  });

  function close() {
    setTyped('');
    setForce(false);
    mutation.reset();
    onClose();
  }

  if (bucket === null) {
    return null;
  }
  const notEmpty = isApiError(mutation.error) && mutation.error.code === 'bucket_not_empty';
  const canForce = perms.can(StorageActions.forceDeleteBucket);
  const confirmed = typed === bucket.name;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (confirmed) {
      mutation.mutate({ name: bucket.name, force });
    }
  };

  return (
    <Modal opened onClose={close} title={t('buckets.deleteTitle', { name: bucket.name })} centered>
      <form onSubmit={submit}>
        <Stack gap="md">
          <Text size="sm">
            {bucket.objects > 0
              ? t('buckets.deleteNotEmpty', {
                  objects: formatNumber(bucket.objects, i18n.resolvedLanguage),
                })
              : t('buckets.deleteEmpty')}
          </Text>
          <Checkbox
            label={t('buckets.forceDelete')}
            description={t('buckets.forceDeleteHint')}
            checked={force}
            disabled={!canForce}
            onChange={(event) => {
              setForce(event.currentTarget.checked);
            }}
          />
          <TextInput
            label={
              <Trans
                i18nKey="buckets.typeToConfirm"
                values={{ name: bucket.name }}
                components={{ code: <code /> }}
              />
            }
            value={typed}
            onChange={(event) => {
              setTyped(event.currentTarget.value);
            }}
            autoComplete="off"
            spellCheck={false}
            data-autofocus
          />
          {mutation.isError ? (
            <Alert color="red" variant="light" role="alert">
              {notEmpty ? t('buckets.notEmptyUseForce') : problemMessage(t, mutation.error)}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" color="red" disabled={!confirmed} loading={mutation.isPending}>
              {t('common.delete')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
