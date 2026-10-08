import {
  Alert,
  Button,
  Checkbox,
  Code,
  Group,
  List,
  Modal,
  Stack,
  Text,
  TextInput,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type DeleteObjectsInput } from '@/services';

import type { BrowserRow } from './rows';

export interface DeleteObjectsDialogProps {
  bucket: string;
  /** Rows to delete; the dialog is closed when empty. */
  rows: readonly BrowserRow[];
  /** The bucket keeps versions: offer to delete them permanently. */
  versioned: boolean;
  onClose: () => void;
  onDeleted?: () => void;
}

/**
 * The text to type to confirm a recursive deletion: the folder itself for
 * one folder, the bucket name for several (null when nothing is recursive).
 */
// eslint-disable-next-line react-refresh/only-export-components -- pure helper tested with the dialog
export function confirmationText(bucket: string, rows: readonly BrowserRow[]): string | null {
  const folders = rows.filter((r) => r.kind === 'folder');
  if (folders.length === 0) return null;
  return folders.length === 1 && folders[0] ? folders[0].key : bucket;
}

/** D-OBJ-04: deletes objects and folders; folders are recursive and need a typed confirmation. */
export function DeleteObjectsDialog({
  bucket,
  rows,
  versioned,
  onClose,
  onDeleted,
}: DeleteObjectsDialogProps) {
  const { t } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const [typed, setTyped] = useState('');
  const [allVersions, setAllVersions] = useState(false);
  const mutation = useMutation({
    mutationFn: (input: DeleteObjectsInput) => objects.remove(bucket, input),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.objects(bucket) });
      await queryClient.invalidateQueries({ queryKey: storageKeys.buckets });
      if (result.errors.length > 0) {
        notifications.show({
          color: 'yellow',
          title: t('objects.deletePartialTitle'),
          message: t('objects.deletePartial', {
            done: result.deleted.length,
            failed: result.errors.length,
            first: result.errors[0]?.message ?? '',
          }),
        });
      } else {
        notifications.show({
          color: 'green',
          message: t('objects.deletedN', { n: result.deleted.length }),
        });
      }
      onDeleted?.();
      close();
    },
  });

  function close() {
    setTyped('');
    setAllVersions(false);
    mutation.reset();
    onClose();
  }

  if (rows.length === 0) {
    return null;
  }
  const expected = confirmationText(bucket, rows);
  const confirmed = expected === null || typed === expected;
  const preview = rows.slice(0, 5);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!confirmed) return;
    mutation.mutate({
      objects: rows.filter((r) => r.kind === 'object').map((r) => ({ key: r.key })),
      prefixes: rows.filter((r) => r.kind === 'folder').map((r) => r.key),
      ...(allVersions ? { allVersions: true } : {}),
    });
  };

  return (
    <Modal opened onClose={close} title={t('objects.deleteTitle', { n: rows.length })} centered>
      <form onSubmit={submit}>
        <Stack gap="md">
          <Text size="sm">{t('objects.deleteIntro')}</Text>
          <List size="sm" spacing={2}>
            {preview.map((row) => (
              <List.Item key={row.id} style={{ overflowWrap: 'anywhere' }}>
                {row.key}
                {row.kind === 'folder' ? ` (${t('objects.recursive')})` : ''}
              </List.Item>
            ))}
            {rows.length > preview.length ? (
              <List.Item>{t('objects.andMore', { n: rows.length - preview.length })}</List.Item>
            ) : null}
          </List>
          {versioned ? (
            <Checkbox
              label={t('objects.allVersions')}
              description={t('objects.allVersionsHint')}
              checked={allVersions}
              onChange={(event) => {
                setAllVersions(event.currentTarget.checked);
              }}
            />
          ) : null}
          {expected !== null ? (
            <TextInput
              label={
                <>
                  {t('objects.typeToConfirm')} <Code>{expected}</Code>
                </>
              }
              value={typed}
              data-autofocus
              onChange={(event) => {
                setTyped(event.currentTarget.value);
              }}
              autoComplete="off"
              spellCheck={false}
            />
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
            <Button type="submit" color="red" disabled={!confirmed} loading={mutation.isPending}>
              {t('common.delete')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
