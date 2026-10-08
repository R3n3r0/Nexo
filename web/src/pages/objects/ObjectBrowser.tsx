import {
  ActionIcon,
  Alert,
  Anchor,
  Box,
  Breadcrumbs,
  Button,
  Group,
  Menu,
  Overlay,
  Paper,
  Skeleton,
  Stack,
  Text,
  TextInput,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import {
  IconAlertCircle,
  IconChevronDown,
  IconDots,
  IconDownload,
  IconEdit,
  IconFileZip,
  IconFolderPlus,
  IconInfoCircle,
  IconRefresh,
  IconSearch,
  IconShare,
  IconTrash,
  IconUpload,
} from '@tabler/icons-react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useLocation, useNavigate, useParams, useSearchParams } from 'react-router';

import { useSession } from '@/auth/SessionProvider';
import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { ShareDialog } from '@/components/share/ShareDialog';
import { downloadUrl } from '@/lib/download';
import { browsePath, prefixFromPathname, prefixSegments } from '@/lib/objectPath';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices } from '@/services';
import { filesFromDataTransfer, filesFromInput, keyFor, type PickedFile } from '@/upload/files';
import { useUploadManager } from '@/upload/UploadProvider';

import { CreateFolderDialog } from './CreateFolderDialog';
import { DeleteObjectsDialog } from './DeleteObjectsDialog';
import { ObjectDrawer } from './ObjectDrawer';
import { ObjectTable } from './ObjectTable';
import { RenameDialog } from './RenameDialog';
import { buildRows, type BrowserRow } from './rows';
import { useObjectListing } from './useObjectListing';

interface ShareTarget {
  key: string;
  prefix: boolean;
  versionId?: string;
}

/**
 * D-OBJ-01..04: the folder view of a bucket. The prefix lives in the route
 * (/buckets/:bucket/browse/<prefix>, each segment percent-encoded) and the
 * object shown in the drawer in ?object=<key>, so every view is a deep link.
 */
export function ObjectBrowser() {
  const { t, i18n } = useTranslation();
  const { bucket = '' } = useParams();
  const location = useLocation();
  const prefix = prefixFromPathname(location.pathname, bucket);
  const [searchParams, setSearchParams] = useSearchParams();
  const openKey = searchParams.get('object');
  const { objects, buckets } = useServices();
  const manager = useUploadManager();
  const perms = usePermissions({ bucket, prefix });
  const listing = useObjectListing(bucket, prefix);
  const details = useQuery({
    queryKey: storageKeys.bucket(bucket),
    queryFn: () => buckets.get(bucket),
  });
  const [filter, setFilter] = useState('');
  const [selection, setSelection] = useState<ReadonlySet<string>>(new Set());
  const [creatingFolder, setCreatingFolder] = useState(false);
  const [renaming, setRenaming] = useState<BrowserRow | null>(null);
  const [deleting, setDeleting] = useState<readonly BrowserRow[]>([]);
  const [sharing, setSharing] = useState<ShareTarget | null>(null);
  const [dragging, setDragging] = useState(false);
  const dragDepth = useRef(0);
  const fileInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement | null>(null);
  const locale = i18n.resolvedLanguage;

  useEffect(() => {
    setSelection(new Set());
    setFilter('');
  }, [bucket, prefix]);

  const pages = listing.data?.pages;
  const rows = useMemo(() => buildRows(pages ?? [], filter), [pages, filter]);
  const selectedRows = useMemo(() => rows.filter((r) => selection.has(r.id)), [rows, selection]);

  const canUpload = perms.can(StorageActions.putObject);
  const canDownload = perms.can(StorageActions.getObject);
  const canDelete = perms.can(StorageActions.deleteObject);
  const versioned =
    details.data?.versioning === 'enabled' || details.data?.versioning === 'suspended';

  // The browser writes the archive to disk as the server streams it.
  const zip = useMutation({
    mutationFn: (input: { keys: string[]; prefixes: string[] }) =>
      objects.downloadZip(bucket, { keys: input.keys, prefixes: input.prefixes, base: prefix }),
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('objects.zipFailed'),
        message: problemMessage(t, error),
      });
    },
  });

  const openObject = useCallback(
    (key: string | null) => {
      setSearchParams(
        (params) => {
          const next = new URLSearchParams(params);
          if (key === null) next.delete('object');
          else next.set('object', key);
          return next;
        },
        { replace: key === null },
      );
    },
    [setSearchParams],
  );

  const enqueue = useCallback(
    (files: readonly PickedFile[]) => {
      if (files.length === 0) return;
      manager.enqueue(
        bucket,
        files.map((f) => ({ key: keyFor(prefix, f.path), file: f.file })),
      );
      notifications.show({ message: t('uploadQueue.queued', { n: files.length }) });
    },
    [manager, bucket, prefix, t],
  );

  const downloadRows = (targets: readonly BrowserRow[]) => {
    const [first] = targets;
    if (targets.length === 1 && first?.kind === 'object') {
      downloadUrl(objects.downloadUrl(bucket, first.key), first.name);
      return;
    }
    const keys = targets.filter((r) => r.kind === 'object').map((r) => r.key);
    const prefixes = targets.filter((r) => r.kind === 'folder').map((r) => r.key);
    // The server names the archive (the folder, or the bucket at the root).
    zip.mutate({ keys, prefixes });
  };

  const onDragEnter = (event: DragEvent) => {
    if (!canUpload || !event.dataTransfer.types.includes('Files')) return;
    event.preventDefault();
    dragDepth.current += 1;
    setDragging(true);
  };
  const onDragOver = (event: DragEvent) => {
    if (!canUpload || !event.dataTransfer.types.includes('Files')) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'copy';
  };
  const onDragLeave = () => {
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (dragDepth.current === 0) setDragging(false);
  };
  const onDrop = (event: DragEvent) => {
    if (!canUpload) return;
    event.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    // The entries must be read before the event handler returns.
    filesFromDataTransfer(event.dataTransfer).then(enqueue, (error: unknown) => {
      notifications.show({ color: 'red', message: problemMessage(t, error) });
    });
  };

  const rowActions = (row: BrowserRow) => (
    <Menu position="bottom-end" withinPortal>
      <Menu.Target>
        <ActionIcon variant="subtle" aria-label={t('objects.actionsFor', { name: row.name })}>
          <IconDots size={16} />
        </ActionIcon>
      </Menu.Target>
      <Menu.Dropdown>
        {row.kind === 'object' ? (
          <Menu.Item
            leftSection={<IconInfoCircle size={16} />}
            onClick={() => {
              openObject(row.key);
            }}
          >
            {t('objects.details')}
          </Menu.Item>
        ) : null}
        <Menu.Item
          leftSection={
            row.kind === 'object' ? <IconDownload size={16} /> : <IconFileZip size={16} />
          }
          disabled={!canDownload}
          onClick={() => {
            downloadRows([row]);
          }}
        >
          {row.kind === 'object' ? t('objects.download') : t('objects.downloadZip')}
        </Menu.Item>
        <Menu.Item
          leftSection={<IconShare size={16} />}
          disabled={!canDownload && !canUpload}
          onClick={() => {
            setSharing({ key: row.key, prefix: row.kind === 'folder' });
          }}
        >
          {t('objects.share')}
        </Menu.Item>
        <Menu.Item
          leftSection={<IconEdit size={16} />}
          disabled={!canUpload || !canDelete}
          onClick={() => {
            setRenaming(row);
          }}
        >
          {t('objects.rename')}
        </Menu.Item>
        <Menu.Divider />
        <Menu.Item
          color="red"
          leftSection={<IconTrash size={16} />}
          disabled={!canDelete}
          onClick={() => {
            setDeleting([row]);
          }}
        >
          {t('common.delete')}
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  );

  const segments = prefixSegments(prefix);

  return (
    <Stack gap="sm">
      <Box component="nav" aria-label={t('objects.breadcrumb')}>
        <Breadcrumbs separator="/" separatorMargin={4} style={{ flexWrap: 'wrap' }}>
          <Anchor component={Link} to={browsePath(bucket)} size="sm">
            {bucket}
          </Anchor>
          {segments.map((segment, index) =>
            index === segments.length - 1 ? (
              <Text
                key={segment.prefix}
                size="sm"
                fw={600}
                aria-current="page"
                style={{ overflowWrap: 'anywhere' }}
              >
                {segment.name}
              </Text>
            ) : (
              <Anchor
                key={segment.prefix}
                component={Link}
                to={browsePath(bucket, segment.prefix)}
                size="sm"
              >
                {segment.name}
              </Anchor>
            ),
          )}
        </Breadcrumbs>
      </Box>

      <Group justify="space-between" gap="xs" wrap="wrap">
        <Group gap="xs" wrap="wrap">
          {canUpload ? (
            <Menu position="bottom-start" withinPortal>
              <Menu.Target>
                <Button
                  leftSection={<IconUpload size={16} />}
                  rightSection={<IconChevronDown size={14} />}
                >
                  {t('objects.upload')}
                </Button>
              </Menu.Target>
              <Menu.Dropdown>
                <Menu.Item
                  onClick={() => {
                    fileInput.current?.click();
                  }}
                >
                  {t('objects.uploadFiles')}
                </Menu.Item>
                <Menu.Item
                  onClick={() => {
                    folderInput.current?.click();
                  }}
                >
                  {t('objects.uploadFolder')}
                </Menu.Item>
              </Menu.Dropdown>
            </Menu>
          ) : null}
          {canUpload ? (
            <Button
              variant="default"
              leftSection={<IconFolderPlus size={16} />}
              onClick={() => {
                setCreatingFolder(true);
              }}
            >
              {t('objects.newFolder')}
            </Button>
          ) : null}
          <PermissionGuard allowed={canDownload} action={StorageActions.getObject}>
            {(disabled) => (
              <Button
                variant="default"
                leftSection={<IconDownload size={16} />}
                disabled={disabled || (selectedRows.length === 0 && rows.length === 0)}
                loading={zip.isPending}
                onClick={() => {
                  if (selectedRows.length > 0) {
                    downloadRows(selectedRows);
                  } else {
                    zip.mutate({ keys: [], prefixes: [prefix] });
                  }
                }}
              >
                {selectedRows.length > 0
                  ? t('objects.downloadSelected', { n: selectedRows.length })
                  : t('objects.downloadFolder')}
              </Button>
            )}
          </PermissionGuard>
          {canDelete ? (
            <Button
              variant="default"
              color="red"
              leftSection={<IconTrash size={16} />}
              disabled={selectedRows.length === 0}
              onClick={() => {
                setDeleting(selectedRows);
              }}
            >
              {t('objects.deleteSelected', { n: selectedRows.length })}
            </Button>
          ) : null}
        </Group>
        <Group gap="xs" wrap="nowrap">
          <TextInput
            leftSection={<IconSearch size={16} />}
            placeholder={t('objects.filterPlaceholder')}
            aria-label={t('objects.filter')}
            value={filter}
            onChange={(event) => {
              setFilter(event.currentTarget.value);
            }}
            w={{ base: 180, sm: 240 }}
          />
          <ActionIcon
            variant="default"
            size="lg"
            aria-label={t('objects.refresh')}
            loading={listing.isRefetching}
            onClick={() => {
              void listing.refetch();
            }}
          >
            <IconRefresh size={16} />
          </ActionIcon>
        </Group>
      </Group>

      <input
        ref={fileInput}
        type="file"
        multiple
        hidden
        data-testid="upload-files-input"
        onChange={(event) => {
          enqueue(filesFromInput(event.currentTarget.files));
          event.currentTarget.value = '';
        }}
      />
      <input
        ref={(node) => {
          folderInput.current = node;
          // Not a React prop: folder pickers are a non-standard attribute.
          node?.setAttribute('webkitdirectory', '');
        }}
        type="file"
        multiple
        hidden
        data-testid="upload-folder-input"
        onChange={(event) => {
          enqueue(filesFromInput(event.currentTarget.files));
          event.currentTarget.value = '';
        }}
      />

      <Paper
        withBorder
        radius="md"
        pos="relative"
        onDragEnter={onDragEnter}
        onDragOver={onDragOver}
        onDragLeave={onDragLeave}
        onDrop={onDrop}
        data-testid="object-dropzone"
      >
        {dragging ? (
          <Overlay
            color="var(--mantine-primary-color-filled)"
            backgroundOpacity={0.12}
            blur={1}
            radius="md"
            zIndex={5}
          >
            <Stack align="center" justify="center" h="100%" gap={4}>
              <IconUpload size={32} />
              <Text fw={600}>
                {t('objects.dropHere', { prefix: prefix === '' ? bucket : prefix })}
              </Text>
            </Stack>
          </Overlay>
        ) : null}
        {listing.isPending ? (
          <Stack p="md" gap="sm" data-testid="objects-loading">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} height={20} />
            ))}
          </Stack>
        ) : listing.isError && isApiError(listing.error) && listing.error.isForbidden ? (
          <ForbiddenFolder key={prefix} bucket={bucket} prefix={prefix} />
        ) : listing.isError ? (
          <Box p="md">
            <Alert color="red" variant="light" icon={<IconAlertCircle size={18} />} role="alert">
              <Group justify="space-between" wrap="nowrap">
                <Text>{problemMessage(t, listing.error)}</Text>
                <Button
                  size="xs"
                  variant="default"
                  onClick={() => {
                    void listing.refetch();
                  }}
                >
                  {t('common.retry')}
                </Button>
              </Group>
            </Alert>
          </Box>
        ) : rows.length === 0 && !listing.hasNextPage ? (
          <Stack align="center" gap={4} p="xl">
            <Text fw={500}>{filter.trim() === '' ? t('objects.empty') : t('objects.noMatch')}</Text>
            {canUpload && filter.trim() === '' ? (
              <Text size="sm" c="dimmed">
                {t('objects.emptyHint')}
              </Text>
            ) : null}
          </Stack>
        ) : (
          <ObjectTable
            rows={rows}
            selection={selection}
            onToggle={(row) => {
              setSelection((current) => {
                const next = new Set(current);
                if (next.has(row.id)) next.delete(row.id);
                else next.add(row.id);
                return next;
              });
            }}
            onToggleAll={(checked) => {
              setSelection(checked ? new Set(rows.map((r) => r.id)) : new Set());
            }}
            folderHref={(row) => browsePath(bucket, row.key)}
            onOpen={(row) => {
              openObject(row.key);
            }}
            renderActions={rowActions}
            hasNextPage={listing.hasNextPage}
            isFetchingNextPage={listing.isFetchingNextPage}
            fetchNextPage={() => {
              void listing.fetchNextPage();
            }}
            locale={locale}
          />
        )}
      </Paper>

      <ObjectDrawer
        bucket={bucket}
        objectKey={openKey}
        perms={perms}
        onClose={() => {
          openObject(null);
        }}
        onShare={(key, versionId) => {
          setSharing({ key, prefix: false, ...(versionId !== undefined ? { versionId } : {}) });
        }}
      />
      <CreateFolderDialog
        opened={creatingFolder}
        bucket={bucket}
        prefix={prefix}
        onClose={() => {
          setCreatingFolder(false);
        }}
      />
      <RenameDialog
        bucket={bucket}
        row={renaming}
        onClose={() => {
          setRenaming(null);
        }}
      />
      <DeleteObjectsDialog
        bucket={bucket}
        rows={deleting}
        versioned={versioned}
        onClose={() => {
          setDeleting([]);
        }}
        onDeleted={() => {
          setSelection(new Set());
        }}
      />
      {sharing !== null ? (
        <ShareDialog
          opened
          bucket={bucket}
          objectKey={sharing.key}
          prefix={sharing.prefix}
          {...(sharing.versionId !== undefined ? { versionId: sharing.versionId } : {})}
          onClose={() => {
            setSharing(null);
          }}
        />
      ) : null}
    </Stack>
  );
}

/**
 * The folder cannot be listed (403): a requester restricted to part of the
 * bucket, such as a home directory (docs/09 §9.3), opens a folder it may
 * list by path instead.
 */
function ForbiddenFolder({ bucket, prefix }: { bucket: string; prefix: string }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { session } = useSession();
  const [path, setPath] = useState(prefix);
  const user = session?.accessKey ?? '';
  const open = () => {
    const trimmed = path.trim().replace(/^\/+/, '');
    if (trimmed === '') return;
    void navigate(browsePath(bucket, trimmed.endsWith('/') ? trimmed : `${trimmed}/`));
  };
  return (
    <Box p="md">
      <Alert color="yellow" variant="light" icon={<IconAlertCircle size={18} />} role="alert">
        <Stack gap="sm">
          <Text>{t('objects.forbiddenFolder')}</Text>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              open();
            }}
          >
            <Group gap="xs" align="flex-end" wrap="wrap">
              <TextInput
                label={t('objects.goToFolder')}
                description={user !== '' ? t('objects.goToFolderHint', { user }) : undefined}
                value={path}
                onChange={(event) => {
                  setPath(event.currentTarget.value);
                }}
                w={{ base: '100%', sm: 320 }}
              />
              <Button type="submit" variant="default">
                {t('objects.goToFolderSubmit')}
              </Button>
            </Group>
          </form>
        </Stack>
      </Alert>
    </Box>
  );
}
