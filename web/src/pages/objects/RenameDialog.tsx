import { Alert, Button, Checkbox, Group, Modal, Stack, TextInput } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices } from '@/services';

import type { BrowserRow } from './rows';

export interface RenameDialogProps {
  bucket: string;
  /** The object or folder to move; closed when null. */
  row: BrowserRow | null;
  onClose: () => void;
}

/** Validates the destination of a move; returns an issue key or null. */
// eslint-disable-next-line react-refresh/only-export-components -- pure helper tested with the dialog
export function destinationIssue(
  row: BrowserRow,
  destination: string,
): 'required' | 'same' | 'inside' | 'slash' | null {
  if (destination.trim() === '' || destination === '/') return 'required';
  if (destination.startsWith('/')) return 'slash';
  if (row.kind === 'folder') {
    const target = destination.endsWith('/') ? destination : `${destination}/`;
    if (target === row.key) return 'same';
    if (target.startsWith(row.key)) return 'inside';
    return null;
  }
  return destination === row.key ? 'same' : null;
}

/** D-OBJ-04: rename or move an object or a folder (copy then delete on the server). */
export function RenameDialog({ bucket, row, onClose }: RenameDialogProps) {
  const { t } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const [destination, setDestination] = useState('');
  const [overwrite, setOverwrite] = useState(false);

  useEffect(() => {
    setDestination(row?.key ?? '');
    setOverwrite(false);
  }, [row]);

  const mutation = useMutation({
    mutationFn: (input: {
      source: string;
      destination: string;
      prefix: boolean;
      overwrite: boolean;
    }) => objects.rename(bucket, input),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.objects(bucket) });
      if (result.errors.length > 0) {
        notifications.show({
          color: 'yellow',
          message: t('objects.renamePartial', {
            done: result.renamed.length,
            failed: result.errors.length,
          }),
        });
      } else {
        notifications.show({
          color: 'green',
          message: t('objects.renamed', { n: result.renamed.length }),
        });
      }
      close();
    },
  });

  function close() {
    mutation.reset();
    onClose();
  }

  if (row === null) {
    return null;
  }
  const issue = destinationIssue(row, destination);
  const conflict = isApiError(mutation.error) && mutation.error.isConflict;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (issue !== null) return;
    const target =
      row.kind === 'folder' && !destination.endsWith('/') ? `${destination}/` : destination;
    mutation.mutate({
      source: row.key,
      destination: target,
      prefix: row.kind === 'folder',
      overwrite,
    });
  };

  return (
    <Modal opened onClose={close} title={t('objects.renameTitle', { name: row.name })} centered>
      <form onSubmit={submit} noValidate>
        <Stack gap="md">
          <TextInput
            label={
              row.kind === 'folder' ? t('objects.destinationFolder') : t('objects.destinationKey')
            }
            description={t('objects.destinationHint')}
            value={destination}
            data-autofocus
            onChange={(event) => {
              setDestination(event.currentTarget.value);
              mutation.reset();
            }}
            error={
              issue !== null && destination !== row.key
                ? t(`objects.destinationIssues.${issue}`)
                : undefined
            }
            autoComplete="off"
            spellCheck={false}
          />
          <Checkbox
            label={t('objects.overwrite')}
            checked={overwrite}
            onChange={(event) => {
              setOverwrite(event.currentTarget.checked);
            }}
          />
          {mutation.isError ? (
            <Alert color="red" variant="light" role="alert">
              {conflict ? t('objects.destinationExists') : problemMessage(t, mutation.error)}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" disabled={issue !== null} loading={mutation.isPending}>
              {t('objects.move')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
