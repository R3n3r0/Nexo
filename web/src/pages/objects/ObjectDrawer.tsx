import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Drawer,
  Group,
  ScrollArea,
  Skeleton,
  Stack,
  Table,
  Tabs,
  Text,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconCheck, IconDownload, IconShare, IconTrash, IconX } from '@tabler/icons-react';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, type PermissionCheck } from '@/auth/usePermissions';
import { TagEditor } from '@/components/storage/TagEditor';
import { downloadUrl } from '@/lib/download';
import { formatBytes } from '@/lib/format';
import { baseName } from '@/lib/objectPath';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type ObjectEntry, type TagSet } from '@/services';

import { ObjectPreview } from './ObjectPreview';

export interface ObjectDrawerProps {
  bucket: string;
  /** Key of the object shown; the drawer is closed when null. */
  objectKey: string | null;
  perms: PermissionCheck;
  onClose: () => void;
  onShare?: (key: string, versionId?: string) => void;
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Table.Tr>
      <Table.Th w={150} style={{ verticalAlign: 'top' }}>
        {label}
      </Table.Th>
      <Table.Td style={{ overflowWrap: 'anywhere' }}>{children}</Table.Td>
    </Table.Tr>
  );
}

function VersionRow({
  bucket,
  version,
  canDelete,
}: {
  bucket: string;
  version: ObjectEntry;
  canDelete: boolean;
}) {
  const { t, i18n } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const versionId = version.versionId ?? 'null';
  const remove = useMutation({
    mutationFn: () => objects.remove(bucket, { objects: [{ key: version.key, versionId }] }),
    onSuccess: async (result) => {
      setConfirming(false);
      await queryClient.invalidateQueries({ queryKey: storageKeys.objects(bucket) });
      if (result.errors.length > 0) {
        notifications.show({ color: 'red', message: result.errors[0]?.message ?? '' });
      } else {
        notifications.show({ color: 'green', message: t('objectVersions.deleted') });
      }
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });
  const locale = i18n.resolvedLanguage;
  return (
    <Table.Tr>
      <Table.Td>
        <Stack gap={2}>
          <Group gap={4}>
            <Text size="xs" ff="monospace" truncate="end" maw={160} title={versionId}>
              {versionId}
            </Text>
            {version.isLatest ? (
              <Badge size="xs" variant="light">
                {t('objectVersions.latest')}
              </Badge>
            ) : null}
            {version.isDeleteMarker ? (
              <Badge size="xs" variant="light" color="red">
                {t('objectVersions.deleteMarker')}
              </Badge>
            ) : null}
          </Group>
          <Text size="xs" c="dimmed">
            {new Date(version.lastModified).toLocaleString(locale)}
            {version.isDeleteMarker ? '' : ` · ${formatBytes(version.size, locale)}`}
          </Text>
        </Stack>
      </Table.Td>
      <Table.Td w={96}>
        <Group gap={4} wrap="nowrap" justify="flex-end">
          {confirming ? (
            <>
              <ActionIcon
                color="red"
                variant="filled"
                size="sm"
                loading={remove.isPending}
                aria-label={t('objectVersions.confirmDelete', { version: versionId })}
                onClick={() => {
                  remove.mutate();
                }}
              >
                <IconCheck size={14} />
              </ActionIcon>
              <ActionIcon
                variant="subtle"
                size="sm"
                aria-label={t('common.cancel')}
                onClick={() => {
                  setConfirming(false);
                }}
              >
                <IconX size={14} />
              </ActionIcon>
            </>
          ) : (
            <>
              {!version.isDeleteMarker ? (
                <Tooltip label={t('objectVersions.download')}>
                  <ActionIcon
                    variant="subtle"
                    size="sm"
                    aria-label={t('objectVersions.downloadVersion', { version: versionId })}
                    onClick={() => {
                      downloadUrl(
                        objects.downloadUrl(bucket, version.key, versionId),
                        baseName(version.key),
                      );
                    }}
                  >
                    <IconDownload size={14} />
                  </ActionIcon>
                </Tooltip>
              ) : null}
              <Tooltip
                label={
                  canDelete
                    ? t('objectVersions.delete')
                    : t('permissions.missing', { action: StorageActions.deleteObjectVersion })
                }
              >
                <ActionIcon
                  variant="subtle"
                  color="red"
                  size="sm"
                  disabled={!canDelete}
                  aria-label={t('objectVersions.deleteVersion', { version: versionId })}
                  onClick={() => {
                    setConfirming(true);
                  }}
                >
                  <IconTrash size={14} />
                </ActionIcon>
              </Tooltip>
            </>
          )}
        </Group>
      </Table.Td>
    </Table.Tr>
  );
}

function VersionsPanel({
  bucket,
  objectKey,
  canDelete,
}: {
  bucket: string;
  objectKey: string;
  canDelete: boolean;
}) {
  const { t } = useTranslation();
  const { objects } = useServices();
  const query = useInfiniteQuery({
    queryKey: storageKeys.versions(bucket, objectKey),
    queryFn: ({ pageParam }) =>
      objects.versions(bucket, objectKey, pageParam !== '' ? { cursor: pageParam } : {}),
    initialPageParam: '',
    getNextPageParam: (last) =>
      last.nextCursor !== undefined && last.nextCursor !== '' ? last.nextCursor : undefined,
  });
  if (query.isPending) {
    return <Skeleton height={120} />;
  }
  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {problemMessage(t, query.error)}
      </Alert>
    );
  }
  const versions = query.data.pages.flatMap((p) => p.items);
  return (
    <Stack gap="xs">
      <Table aria-label={t('objectVersions.title')} verticalSpacing={6}>
        <Table.Tbody>
          {versions.map((version) => (
            <VersionRow
              key={version.versionId ?? 'null'}
              bucket={bucket}
              version={version}
              canDelete={canDelete}
            />
          ))}
        </Table.Tbody>
      </Table>
      {query.hasNextPage ? (
        <Button
          size="xs"
          variant="subtle"
          loading={query.isFetchingNextPage}
          onClick={() => {
            void query.fetchNextPage();
          }}
        >
          {t('objects.loadMore')}
        </Button>
      ) : null}
    </Stack>
  );
}

/** D-OBJ-06: metadata, preview, tags and versions of an object. */
export function ObjectDrawer({ bucket, objectKey, perms, onClose, onShare }: ObjectDrawerProps) {
  const { t, i18n } = useTranslation();
  const { objects } = useServices();
  const queryClient = useQueryClient();
  const [tab, setTab] = useState<string | null>('details');
  const key = objectKey ?? '';
  const stat = useQuery({
    queryKey: storageKeys.object(bucket, key),
    queryFn: () => objects.stat(bucket, key),
    enabled: objectKey !== null,
  });
  const saveTags = useMutation({
    mutationFn: (tags: TagSet) => objects.setTags(bucket, key, tags),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.object(bucket, key) });
      notifications.show({ color: 'green', message: t('bucketSettings.saved') });
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });
  const locale = i18n.resolvedLanguage;
  const details = stat.data;
  const canRead = perms.can(StorageActions.getObject);

  return (
    <Drawer
      opened={objectKey !== null}
      onClose={() => {
        setTab('details');
        onClose();
      }}
      position="right"
      size="lg"
      title={
        <Text fw={600} style={{ overflowWrap: 'anywhere' }}>
          {baseName(key)}
        </Text>
      }
      scrollAreaComponent={ScrollArea.Autosize}
      closeButtonProps={{ 'aria-label': t('common.close') }}
    >
      {stat.isPending && objectKey !== null ? (
        <Skeleton height={240} />
      ) : stat.isError ? (
        <Alert color="red" variant="light" role="alert">
          {problemMessage(t, stat.error)}
        </Alert>
      ) : details ? (
        <Stack gap="md">
          <Group gap="xs">
            <Button
              size="xs"
              leftSection={<IconDownload size={14} />}
              disabled={!canRead}
              onClick={() => {
                downloadUrl(objects.downloadUrl(bucket, key), baseName(key));
              }}
            >
              {t('objects.download')}
            </Button>
            {onShare ? (
              <Button
                size="xs"
                variant="default"
                leftSection={<IconShare size={14} />}
                disabled={!canRead}
                onClick={() => {
                  onShare(key);
                }}
              >
                {t('objects.share')}
              </Button>
            ) : null}
          </Group>
          <Tabs value={tab} onChange={setTab} keepMounted={false}>
            <Tabs.List>
              <Tabs.Tab value="details">{t('objectDetails.tabs.details')}</Tabs.Tab>
              <Tabs.Tab value="preview">{t('objectDetails.tabs.preview')}</Tabs.Tab>
              <Tabs.Tab value="tags">{t('objectDetails.tabs.tags')}</Tabs.Tab>
              <Tabs.Tab value="versions">{t('objectDetails.tabs.versions')}</Tabs.Tab>
            </Tabs.List>
            <Tabs.Panel value="details" pt="md">
              <Table variant="vertical" layout="fixed" withTableBorder>
                <Table.Tbody>
                  <Field label={t('objectDetails.key')}>
                    <Text size="sm" ff="monospace">
                      {details.key}
                    </Text>
                  </Field>
                  <Field label={t('objects.size')}>
                    {formatBytes(details.size, locale)} ({details.size.toLocaleString(locale)} B)
                  </Field>
                  <Field label={t('objects.modified')}>
                    {new Date(details.lastModified).toLocaleString(locale)}
                  </Field>
                  <Field label={t('objectDetails.contentType')}>{details.contentType ?? '—'}</Field>
                  <Field label={t('objectDetails.etag')}>
                    <Text size="sm" ff="monospace">
                      {details.etag}
                    </Text>
                  </Field>
                  {details.versionId !== undefined ? (
                    <Field label={t('objectDetails.versionId')}>
                      <Text size="sm" ff="monospace">
                        {details.versionId}
                      </Text>
                    </Field>
                  ) : null}
                  {details.storageClass !== undefined ? (
                    <Field label={t('objectDetails.storageClass')}>{details.storageClass}</Field>
                  ) : null}
                  {details.cacheControl !== undefined ? (
                    <Field label={t('objectDetails.cacheControl')}>{details.cacheControl}</Field>
                  ) : null}
                  {details.contentDisposition !== undefined ? (
                    <Field label={t('objectDetails.contentDisposition')}>
                      {details.contentDisposition}
                    </Field>
                  ) : null}
                  {details.contentEncoding !== undefined ? (
                    <Field label={t('objectDetails.contentEncoding')}>
                      {details.contentEncoding}
                    </Field>
                  ) : null}
                  {details.parts !== undefined && details.parts > 0 ? (
                    <Field label={t('objectDetails.parts')}>{details.parts}</Field>
                  ) : null}
                  {details.checksum !== undefined ? (
                    <Field label={t('objectDetails.checksum')}>
                      {details.checksumAlgorithm ?? ''} {details.checksum}
                    </Field>
                  ) : null}
                  {Object.entries(details.userMetadata).map(([name, value]) => (
                    <Field key={name} label={`x-amz-meta-${name}`}>
                      {value}
                    </Field>
                  ))}
                </Table.Tbody>
              </Table>
            </Tabs.Panel>
            <Tabs.Panel value="preview" pt="md">
              {canRead ? (
                <ObjectPreview
                  bucket={bucket}
                  objectKey={key}
                  contentType={details.contentType}
                  size={details.size}
                />
              ) : (
                <Text size="sm" c="dimmed">
                  {t('permissions.missing', { action: StorageActions.getObject })}
                </Text>
              )}
            </Tabs.Panel>
            <Tabs.Panel value="tags" pt="md">
              {details.tags === undefined ? (
                <Text size="sm" c="dimmed">
                  {t('objectDetails.tagsHidden')}
                </Text>
              ) : (
                <TagEditor
                  tags={details.tags}
                  canEdit={perms.can(StorageActions.putObjectTagging)}
                  action={StorageActions.putObjectTagging}
                  saving={saveTags.isPending}
                  onSave={(tags) => {
                    saveTags.mutate(tags);
                  }}
                />
              )}
            </Tabs.Panel>
            <Tabs.Panel value="versions" pt="md">
              {perms.can(StorageActions.listVersions) ? (
                <VersionsPanel
                  bucket={bucket}
                  objectKey={key}
                  canDelete={perms.can(StorageActions.deleteObjectVersion)}
                />
              ) : (
                <Text size="sm" c="dimmed">
                  {t('permissions.missing', { action: StorageActions.listVersions })}
                </Text>
              )}
            </Tabs.Panel>
          </Tabs>
        </Stack>
      ) : null}
    </Drawer>
  );
}
