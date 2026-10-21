import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  FileInput,
  Group,
  Menu,
  Modal,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconDownload, IconEdit, IconTrash, IconUpload } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, type PermissionCheck } from '@/auth/usePermissions';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { formatNumber } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { canonicalLanguage, maxSubtitleBytes } from '@/lib/subtitles';
import { useServices, type SubtitleTrack } from '@/services';

import { SubtitleEditor } from './SubtitleEditor';

/** Reads a file as UTF-8 text. */
function readText(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      resolve(typeof reader.result === 'string' ? reader.result : '');
    };
    reader.onerror = () => {
      reject(reader.error ?? new Error('read failed'));
    };
    reader.readAsText(file);
  });
}

/** Uploads a WebVTT or SubRip file as the manual track of a language. */
function UploadDialog({
  opened,
  onClose,
  bucket,
  objectKey,
  versionId,
}: {
  opened: boolean;
  onClose: () => void;
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
}) {
  const { t } = useTranslation();
  const { media } = useServices();
  const queryClient = useQueryClient();
  const [lang, setLang] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const tag = canonicalLanguage(lang);
  const tooLarge = file !== null && file.size > maxSubtitleBytes;
  const upload = useMutation({
    mutationFn: async () => {
      if (file === null || tag === undefined) throw new Error('unreachable');
      return media.uploadSubtitles(bucket, objectKey, tag, await readText(file), versionId);
    },
    onSuccess: async (track) => {
      notifications.show({
        color: 'green',
        message: t('subtitles.uploaded', { name: track.name, n: track.cues }),
      });
      await queryClient.invalidateQueries({
        queryKey: storageKeys.subtitles(bucket, objectKey, versionId),
      });
      setLang('');
      setFile(null);
      onClose();
    },
  });

  return (
    <Modal opened={opened} onClose={onClose} title={t('subtitles.upload')} centered>
      <Stack gap="sm">
        <TextInput
          label={t('subtitles.language')}
          description={t('subtitles.languageHint')}
          placeholder="it, en, pt-BR"
          value={lang}
          onChange={(e) => {
            setLang(e.currentTarget.value);
          }}
          error={lang !== '' && tag === undefined ? t('subtitles.invalidLanguage') : undefined}
          data-autofocus
        />
        <FileInput
          label={t('subtitles.file')}
          description={t('subtitles.fileHint')}
          accept=".vtt,.srt,text/vtt,application/x-subrip"
          value={file}
          onChange={setFile}
          clearable
          error={tooLarge ? t('subtitles.tooLarge') : undefined}
        />
        {upload.error !== null ? <ErrorAlert error={upload.error} /> : null}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onClose} disabled={upload.isPending}>
            {t('common.cancel')}
          </Button>
          <Button
            disabled={tag === undefined || file === null || tooLarge}
            loading={upload.isPending}
            onClick={() => {
              upload.mutate();
            }}
          >
            {t('subtitles.uploadAction')}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

export interface SubtitlesPanelProps {
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
  perms: PermissionCheck;
  /** Why the automatic subtitles failed (MediaStatus.subtitlesError). */
  automaticError?: string | undefined;
}

/**
 * The subtitle tracks of a video: manual (uploaded or edited) and
 * automatic (transcribed or translated by the media worker). They play as
 * captions in every HLS player.
 */
export function SubtitlesPanel({
  bucket,
  objectKey,
  versionId,
  perms,
  automaticError,
}: SubtitlesPanelProps) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { media } = useServices();
  const queryClient = useQueryClient();
  const key = storageKeys.subtitles(bucket, objectKey, versionId);
  const query = useQuery({
    queryKey: key,
    queryFn: () => media.subtitles(bucket, objectKey, versionId),
  });
  const [uploading, setUploading] = useState(false);
  const [editing, setEditing] = useState<string | undefined>(undefined);
  const [removing, setRemoving] = useState<SubtitleTrack | undefined>(undefined);
  const remove = useMutation({
    mutationFn: (lang: string) => media.deleteSubtitles(bucket, objectKey, lang, versionId),
    onSuccess: async () => {
      setRemoving(undefined);
      await queryClient.invalidateQueries({ queryKey: key });
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });
  const canWrite = perms.can(StorageActions.putObject);
  const origin = (tr: SubtitleTrack) => {
    if (!tr.auto) return t('subtitles.manual');
    if (tr.from !== undefined && tr.from !== '')
      return t('subtitles.translated', { from: tr.from });
    return t('subtitles.transcribed');
  };

  return (
    <Stack gap="xs">
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Text size="sm" fw={500}>
          {t('subtitles.title')}
        </Text>
        <PermissionGuard allowed={canWrite} action={StorageActions.putObject}>
          {(disabled) => (
            <Button
              size="xs"
              variant="default"
              leftSection={<IconUpload size={14} />}
              disabled={disabled}
              onClick={() => {
                setUploading(true);
              }}
            >
              {t('subtitles.upload')}
            </Button>
          )}
        </PermissionGuard>
      </Group>
      {automaticError !== undefined && automaticError !== '' ? (
        <Alert color="orange" variant="light" title={t('subtitles.automaticFailed')}>
          <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
            {automaticError}
          </Text>
        </Alert>
      ) : null}
      {query.isPending ? <Skeleton height={60} /> : null}
      {query.isError ? <ErrorAlert error={query.error} /> : null}
      {query.data !== undefined && query.data.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t('subtitles.none')}
        </Text>
      ) : null}
      {query.data !== undefined && query.data.length > 0 ? (
        <Table.ScrollContainer minWidth={480}>
          <Table aria-label={t('subtitles.title')} withTableBorder verticalSpacing={4}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('subtitles.language')}</Table.Th>
                <Table.Th>{t('subtitles.origin')}</Table.Th>
                <Table.Th>{t('subtitles.cues')}</Table.Th>
                <Table.Th>{t('subtitles.updated')}</Table.Th>
                <Table.Th>
                  <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                    {t('common.actions')}
                  </Text>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {query.data.map((tr) => (
                <Table.Tr key={tr.lang}>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <Text size="sm">{tr.name}</Text>
                      <Badge variant="light" color="gray" tt="none" size="sm">
                        {tr.lang}
                      </Badge>
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Badge variant="light" color={tr.auto ? 'grape' : 'blue'} tt="none">
                      {origin(tr)}
                    </Badge>
                  </Table.Td>
                  <Table.Td>{formatNumber(tr.cues, locale)}</Table.Td>
                  <Table.Td>
                    <Text size="xs" c="dimmed">
                      {tr.updated !== undefined ? new Date(tr.updated).toLocaleString(locale) : ''}
                      {tr.updatedBy !== undefined && tr.updatedBy !== ''
                        ? ` · ${tr.updatedBy}`
                        : ''}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={2} wrap="nowrap" justify="flex-end">
                      <Tooltip label={t('subtitles.editTrack', { name: tr.name })}>
                        <ActionIcon
                          variant="subtle"
                          aria-label={t('subtitles.editTrack', { name: tr.name })}
                          disabled={!canWrite}
                          onClick={() => {
                            setEditing(tr.lang);
                          }}
                        >
                          <IconEdit size={16} />
                        </ActionIcon>
                      </Tooltip>
                      <Menu position="bottom-end" withinPortal>
                        <Menu.Target>
                          <Tooltip label={t('subtitles.download', { name: tr.name })}>
                            <ActionIcon
                              variant="subtle"
                              aria-label={t('subtitles.download', { name: tr.name })}
                            >
                              <IconDownload size={16} />
                            </ActionIcon>
                          </Tooltip>
                        </Menu.Target>
                        <Menu.Dropdown>
                          {(['vtt', 'srt'] as const).map((format) => (
                            <Menu.Item
                              key={format}
                              component="a"
                              href={media.subtitleDownloadUrl(
                                bucket,
                                objectKey,
                                tr.lang,
                                format,
                                versionId,
                              )}
                              download={`${tr.lang}.${format}`}
                            >
                              {t(`subtitles.format.${format}`)}
                            </Menu.Item>
                          ))}
                        </Menu.Dropdown>
                      </Menu>
                      <Tooltip label={t('subtitles.remove', { name: tr.name })}>
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          aria-label={t('subtitles.remove', { name: tr.name })}
                          disabled={!canWrite}
                          onClick={() => {
                            setRemoving(tr);
                          }}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Tooltip>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      ) : null}
      <Text size="xs" c="dimmed">
        {t('subtitles.hint')}
      </Text>

      <UploadDialog
        opened={uploading}
        onClose={() => {
          setUploading(false);
        }}
        bucket={bucket}
        objectKey={objectKey}
        versionId={versionId}
      />
      <SubtitleEditor
        bucket={bucket}
        objectKey={objectKey}
        versionId={versionId}
        lang={editing}
        onClose={() => {
          setEditing(undefined);
        }}
      />
      <ConfirmDialog
        opened={removing !== undefined}
        title={t('subtitles.removeTitle')}
        message={
          removing === undefined
            ? ''
            : removing.auto
              ? t('subtitles.removeAutomatic', { name: removing.name })
              : t('subtitles.removeManual', { name: removing.name })
        }
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          if (removing !== undefined) remove.mutate(removing.lang);
        }}
        onCancel={() => {
          setRemoving(undefined);
        }}
      />
    </Stack>
  );
}
