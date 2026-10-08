import { Alert, Button, Group, Modal, Stack, Text, TextInput } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { folderNameIssue } from '@/lib/objectPath';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices } from '@/services';

export interface CreateFolderDialogProps {
  opened: boolean;
  bucket: string;
  /** The folder the new one is created in. */
  prefix: string;
  onClose: () => void;
  onCreated?: (prefix: string) => void;
}

/** Creates the empty object "prefix/name/" so that the folder exists before any upload. */
export function CreateFolderDialog({
  opened,
  bucket,
  prefix,
  onClose,
  onCreated,
}: CreateFolderDialogProps) {
  const { t } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const [name, setName] = useState('');
  const [touched, setTouched] = useState(false);
  const issue = folderNameIssue(name);
  const target = `${prefix}${name.replace(/\/+$/, '')}/`;
  const mutation = useMutation({
    mutationFn: () => objects.createFolder(bucket, target),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.objects(bucket) });
      notifications.show({ color: 'green', message: t('objects.folderCreated', { name: target }) });
      onCreated?.(target);
      close();
    },
  });

  function close() {
    setName('');
    setTouched(false);
    mutation.reset();
    onClose();
  }

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setTouched(true);
    if (issue === null) {
      mutation.mutate();
    }
  };

  return (
    <Modal opened={opened} onClose={close} title={t('objects.newFolder')} centered>
      <form onSubmit={submit} noValidate>
        <Stack gap="md">
          <TextInput
            label={t('objects.folderName')}
            description={t('objects.folderNameHint')}
            required
            data-autofocus
            value={name}
            onChange={(event) => {
              setName(event.currentTarget.value);
              setTouched(true);
            }}
            error={touched && issue !== null ? t(`objects.folderIssues.${issue}`) : undefined}
            autoComplete="off"
          />
          {issue === null ? (
            <Text size="xs" c="dimmed" style={{ overflowWrap: 'anywhere' }}>
              {t('objects.folderPreview', { bucket, key: target })}
            </Text>
          ) : null}
          {mutation.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, mutation.error)}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              {t('common.create')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
