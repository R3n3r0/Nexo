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
import {
  IconCheck,
  IconDownload,
  IconHistory,
  IconLock,
  IconShare,
  IconTrash,
  IconX,
} from '@tabler/icons-react';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { lazy, Suspense, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, type PermissionCheck } from '@/auth/usePermissions';
import { loadSectionTranslations } from '@/i18n';
import { TagEditor } from '@/components/storage/TagEditor';
import { downloadUrl } from '@/lib/download';
import { formatBytes } from '@/lib/format';
import { canStream } from '@/lib/media';
import { isMediaImage } from '@/lib/mediaRules';
import { baseName } from '@/lib/objectPath';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type ObjectEncryption, type ObjectEntry, type TagSet } from '@/services';

import { ObjectAttributesPanel } from './ObjectAttributesPanel';
import { ObjectLockPanel } from './ObjectLockPanel';
import { ObjectPreview } from './ObjectPreview';
import { VersionDeleteDialog } from './VersionDeleteDialog';

// The outputs of the media worker: a chunk of its own, with the strings of
// the sections loaded on demand (i18n sections).
const ObjectMediaPanel = lazy(async () => {
  const [module] = await Promise.all([import('./ObjectMediaPanel'), loadSectionTranslations()]);
  return { default: module.ObjectMediaPanel };
});

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

/** The encryption of a version in words. */
function EncryptionText({ encryption }: { encryption: ObjectEncryption | undefined }) {
  const { t } = useTranslation();
  if (encryption === undefined) {
    return <>{t('objectDetails.notEncrypted')}</>;
  }
  return (
    <Group gap="xs">
      <Badge variant="light" color="teal">
        {encryption.type}
      </Badge>
      {encryption.kmsKeyId !== undefined && encryption.kmsKeyId !== '' ? (
        <Text size="sm" ff="monospace">
          {t('objectDetails.kmsKey', { key: encryption.kmsKeyId })}
        </Text>
      ) : null}
    </Group>
  );
}

function VersionRow({
  bucket,
  version,
  canDelete,
  canRestore,
  onDelete,
}: {
  bucket: string;
  version: ObjectEntry;
  canDelete: boolean;
  canRestore: boolean;
  /** Deletes through the Object Lock dialog instead of the inline confirmation. */
  onDelete?: (() => void) | undefined;
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
  const restore = useMutation({
    mutationFn: () => objects.restoreVersion(bucket, version.key, versionId),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.objects(bucket) });
      await queryClient.invalidateQueries({ queryKey: storageKeys.versions(bucket, version.key) });
      await queryClient.invalidateQueries({ queryKey: storageKeys.object(bucket, version.key) });
      notifications.show({
        color: 'green',
        message: t('objectVersions.restored', { version: versionId }),
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
  const locale = i18n.resolvedLanguage;
  return (
    <Table.Tr
      data-delete-marker={version.isDeleteMarker || undefined}
      style={
        version.isDeleteMarker ? { backgroundColor: 'var(--mantine-color-red-light)' } : undefined
      }
    >
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
      <Table.Td w={120}>
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
              {!version.isDeleteMarker && version.isLatest !== true ? (
                <Tooltip
                  label={
                    canRestore
                      ? t('objectVersions.restore')
                      : t('permissions.missing', { action: StorageActions.getObjectVersion })
                  }
                >
                  <ActionIcon
                    variant="subtle"
                    size="sm"
                    disabled={!canRestore}
                    loading={restore.isPending}
                    aria-label={t('objectVersions.restoreVersion', { version: versionId })}
                    onClick={() => {
                      restore.mutate();
                    }}
                  >
                    <IconHistory size={14} />
                  </ActionIcon>
                </Tooltip>
              ) : null}
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
                    if (onDelete !== undefined) onDelete();
                    else setConfirming(true);
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
  canRestore,
  locked,
  perms,
}: {
  bucket: string;
  objectKey: string;
  canDelete: boolean;
  canRestore: boolean;
  /** The bucket has Object Lock: deletions go through the lock dialog. */
  locked: boolean;
  perms: PermissionCheck;
}) {
  const { t } = useTranslation();
  const { objects } = useServices();
  const [deleting, setDeleting] = useState<ObjectEntry | null>(null);
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
              canRestore={canRestore}
              onDelete={
                locked
                  ? () => {
                      setDeleting(version);
                    }
                  : undefined
              }
            />
          ))}
        </Table.Tbody>
      </Table>
      {locked ? (
        <Text size="xs" c="dimmed">
          {t('objectVersions.lockedHint')}
        </Text>
      ) : null}
      <VersionDeleteDialog
        bucket={bucket}
        version={deleting}
        perms={perms}
        onClose={() => {
          setDeleting(null);
        }}
      />
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

/**
 * D-OBJ-06: metadata and encryption, preview, tags, versions, Object Lock
 * (retention and legal hold) and attributes (checksum, parts) of an object.
 */
export function ObjectDrawer({ bucket, objectKey, perms, onClose, onShare }: ObjectDrawerProps) {
  const { t, i18n } = useTranslation();
  const { objects, buckets } = useServices();
  const queryClient = useQueryClient();
  const [tab, setTab] = useState<string | null>('details');
  const key = objectKey ?? '';
  const bucketInfo = useQuery({
    queryKey: storageKeys.bucket(bucket),
    queryFn: () => buckets.get(bucket),
    enabled: objectKey !== null,
  });
  const locked = bucketInfo.data?.objectLockEnabled === true;
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
  // The media worker selects videos by suffix: the MP4 family by default.
  const video =
    details !== undefined &&
    (details.contentType?.toLowerCase().startsWith('video/') === true || canStream(key, ''));
  // An image rule selects the images the worker decodes (AVIF ... WebP).
  const image = details !== undefined && !video && isMediaImage(key, details.contentType);
  const media = video || image;

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
              {media ? <Tabs.Tab value="media">{t('objectDetails.tabs.media')}</Tabs.Tab> : null}
              {locked ? (
                <Tabs.Tab value="lock" leftSection={<IconLock size={14} />}>
                  {t('objectDetails.tabs.lock')}
                </Tabs.Tab>
              ) : null}
              <Tabs.Tab value="attributes">{t('objectDetails.tabs.attributes')}</Tabs.Tab>
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
                  <Field label={t('objectDetails.encryption')}>
                    <EncryptionText encryption={details.encryption} />
                  </Field>
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
                  canRestore={
                    perms.can(StorageActions.getObjectVersion) &&
                    perms.can(StorageActions.putObject)
                  }
                  locked={locked}
                  perms={perms}
                />
              ) : (
                <Text size="sm" c="dimmed">
                  {t('permissions.missing', { action: StorageActions.listVersions })}
                </Text>
              )}
            </Tabs.Panel>
            {media ? (
              <Tabs.Panel value="media" pt="md">
                {canRead ? (
                  <Suspense fallback={<Skeleton height={120} />}>
                    <ObjectMediaPanel
                      kind={image ? 'image' : 'video'}
                      bucket={bucket}
                      objectKey={key}
                      versionId={details.versionId}
                      perms={perms}
                    />
                  </Suspense>
                ) : (
                  <Text size="sm" c="dimmed">
                    {t('permissions.missing', { action: StorageActions.getObject })}
                  </Text>
                )}
              </Tabs.Panel>
            ) : null}
            {locked ? (
              <Tabs.Panel value="lock" pt="md">
                <ObjectLockPanel
                  bucket={bucket}
                  objectKey={key}
                  versionId={details.versionId}
                  perms={perms}
                />
              </Tabs.Panel>
            ) : null}
            <Tabs.Panel value="attributes" pt="md">
              {perms.can(StorageActions.getAttributes) ? (
                <ObjectAttributesPanel
                  bucket={bucket}
                  objectKey={key}
                  versionId={details.versionId}
                />
              ) : (
                <Text size="sm" c="dimmed">
                  {t('permissions.missing', { action: StorageActions.getAttributes })}
                </Text>
              )}
            </Tabs.Panel>
          </Tabs>
        </Stack>
      ) : null}
    </Drawer>
  );
}
