import {
  ActionIcon,
  Alert,
  Anchor,
  Badge,
  Button,
  Group,
  Menu,
  Paper,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  UnstyledButton,
  VisuallyHidden,
} from '@mantine/core';
import {
  IconAlertCircle,
  IconChevronDown,
  IconChevronUp,
  IconDots,
  IconFolderOpen,
  IconPlus,
  IconSearch,
  IconSelector,
  IconSettings,
  IconTrash,
} from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { formatBytes, formatNumber } from '@/lib/format';
import { browsePath } from '@/lib/objectPath';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type Bucket } from '@/services';

import { CreateBucketDialog } from './CreateBucketDialog';
import { DeleteBucketDialog } from './DeleteBucketDialog';

export type BucketSortKey = 'name' | 'size' | 'objects' | 'created';
export type SortDirection = 'asc' | 'desc';

/** Filters by name (case-insensitive substring) and sorts the buckets. */
// eslint-disable-next-line react-refresh/only-export-components -- pure helper tested with the page
export function sortBuckets(
  buckets: readonly Bucket[],
  search: string,
  key: BucketSortKey,
  direction: SortDirection,
): Bucket[] {
  const needle = search.trim().toLowerCase();
  const filtered = needle === '' ? [...buckets] : buckets.filter((b) => b.name.includes(needle));
  const sign = direction === 'asc' ? 1 : -1;
  filtered.sort((a, b) => {
    let diff: number;
    switch (key) {
      case 'size':
        diff = a.size - b.size;
        break;
      case 'objects':
        diff = a.objects - b.objects;
        break;
      case 'created':
        diff = Date.parse(a.created) - Date.parse(b.created);
        break;
      default:
        diff = 0;
    }
    return (diff !== 0 ? diff : a.name.localeCompare(b.name)) * sign;
  });
  return filtered;
}

interface SortHeaderProps {
  label: string;
  column: BucketSortKey;
  sort: { key: BucketSortKey; direction: SortDirection };
  onSort: (key: BucketSortKey) => void;
  numeric?: boolean;
}

function SortHeader({ label, column, sort, onSort, numeric = false }: SortHeaderProps) {
  const active = sort.key === column;
  const Icon = !active ? IconSelector : sort.direction === 'asc' ? IconChevronUp : IconChevronDown;
  return (
    <Table.Th
      aria-sort={active ? (sort.direction === 'asc' ? 'ascending' : 'descending') : 'none'}
      style={numeric ? { textAlign: 'right' } : undefined}
    >
      <UnstyledButton
        onClick={() => {
          onSort(column);
        }}
        style={{ display: 'inline-flex', alignItems: 'center', gap: 4, fontWeight: 600 }}
      >
        {label}
        <Icon size={14} aria-hidden />
      </UnstyledButton>
    </Table.Th>
  );
}

/** Row actions; the bucket permissions are fetched when the menu opens. */
function BucketActions({ bucket, onDelete }: { bucket: Bucket; onDelete: (b: Bucket) => void }) {
  const { t } = useTranslation();
  const [opened, setOpened] = useState(false);
  const perms = usePermissions({ bucket: bucket.name }, { enabled: opened });
  return (
    <Menu position="bottom-end" withinPortal opened={opened} onChange={setOpened}>
      <Menu.Target>
        <ActionIcon variant="subtle" aria-label={t('buckets.actionsFor', { name: bucket.name })}>
          <IconDots size={16} />
        </ActionIcon>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Item
          component={Link}
          to={browsePath(bucket.name)}
          leftSection={<IconFolderOpen size={16} />}
        >
          {t('buckets.browse')}
        </Menu.Item>
        <Menu.Item
          component={Link}
          to={`/buckets/${encodeURIComponent(bucket.name)}/settings`}
          leftSection={<IconSettings size={16} />}
        >
          {t('buckets.settings')}
        </Menu.Item>
        <Menu.Divider />
        <Menu.Item
          color="red"
          leftSection={<IconTrash size={16} />}
          disabled={!perms.can(StorageActions.deleteBucket)}
          onClick={() => {
            onDelete(bucket);
          }}
        >
          {perms.ready && !perms.can(StorageActions.deleteBucket)
            ? t('buckets.deleteDenied')
            : t('common.delete')}
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  );
}

/** D-BKT-01: the buckets with search, sorting, creation and deletion. */
export function BucketsPage() {
  const { t, i18n } = useTranslation();
  const { buckets } = useServices();
  const perms = usePermissions();
  const locale = i18n.resolvedLanguage;
  const [search, setSearch] = useState('');
  const [sort, setSort] = useState<{ key: BucketSortKey; direction: SortDirection }>({
    key: 'name',
    direction: 'asc',
  });
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Bucket | null>(null);
  const query = useQuery({ queryKey: storageKeys.buckets, queryFn: () => buckets.list() });

  const rows = useMemo(
    () => sortBuckets(query.data ?? [], search, sort.key, sort.direction),
    [query.data, search, sort],
  );

  const onSort = (key: BucketSortKey) => {
    setSort((current) =>
      current.key === key
        ? { key, direction: current.direction === 'asc' ? 'desc' : 'asc' }
        : { key, direction: key === 'name' ? 'asc' : 'desc' },
    );
  };

  const canCreate = perms.can(StorageActions.createBucket);

  return (
    <Stack gap="lg">
      <Group justify="space-between" align="flex-end" wrap="wrap">
        <Stack gap={2}>
          <Title order={2}>{t('buckets.title')}</Title>
          <Text c="dimmed" size="sm">
            {t('buckets.subtitle')}
          </Text>
        </Stack>
        {canCreate ? (
          <Button
            leftSection={<IconPlus size={16} />}
            onClick={() => {
              setCreating(true);
            }}
          >
            {t('buckets.create')}
          </Button>
        ) : null}
      </Group>

      <TextInput
        leftSection={<IconSearch size={16} />}
        placeholder={t('buckets.searchPlaceholder')}
        aria-label={t('buckets.search')}
        value={search}
        onChange={(event) => {
          setSearch(event.currentTarget.value);
        }}
        maw={360}
      />

      {query.isError ? (
        <Alert color="red" variant="light" icon={<IconAlertCircle size={18} />} role="alert">
          <Group justify="space-between" wrap="nowrap">
            <Text>{t('buckets.loadError')}</Text>
            <Button
              size="xs"
              variant="default"
              onClick={() => {
                void query.refetch();
              }}
            >
              {t('common.retry')}
            </Button>
          </Group>
        </Alert>
      ) : null}

      <Paper withBorder radius="md">
        <Table.ScrollContainer minWidth={640}>
          <Table highlightOnHover verticalSpacing="sm" aria-label={t('buckets.title')}>
            <Table.Thead>
              <Table.Tr>
                <SortHeader label={t('common.name')} column="name" sort={sort} onSort={onSort} />
                <SortHeader
                  label={t('buckets.size')}
                  column="size"
                  sort={sort}
                  onSort={onSort}
                  numeric
                />
                <SortHeader
                  label={t('buckets.objects')}
                  column="objects"
                  sort={sort}
                  onSort={onSort}
                  numeric
                />
                <SortHeader
                  label={t('common.created')}
                  column="created"
                  sort={sort}
                  onSort={onSort}
                />
                <Table.Th>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {query.isPending
                ? [0, 1, 2].map((i) => (
                    <Table.Tr key={i} data-testid="bucket-skeleton">
                      {[0, 1, 2, 3, 4].map((c) => (
                        <Table.Td key={c}>
                          <Skeleton height={16} />
                        </Table.Td>
                      ))}
                    </Table.Tr>
                  ))
                : rows.map((bucket) => (
                    <Table.Tr key={bucket.name}>
                      <Table.Td>
                        <Group gap="xs" wrap="nowrap">
                          <Anchor component={Link} to={browsePath(bucket.name)} fw={500}>
                            {bucket.name}
                          </Anchor>
                          {bucket.public === true ? (
                            <Badge size="sm" color="orange" variant="light">
                              {t('buckets.public')}
                            </Badge>
                          ) : null}
                          {bucket.versioning === 'enabled' ? (
                            <Badge size="sm" variant="outline" color="gray">
                              {t('buckets.versioned')}
                            </Badge>
                          ) : null}
                        </Group>
                      </Table.Td>
                      {bucket.partial === true ? (
                        // Only a part of the bucket may be listed: the
                        // server leaves its usage out.
                        <Table.Td colSpan={2} style={{ textAlign: 'right' }}>
                          <Text size="sm" c="dimmed">
                            {t('bucket.limitedAccess')}
                          </Text>
                        </Table.Td>
                      ) : (
                        <>
                          <Table.Td style={{ textAlign: 'right' }}>
                            {formatBytes(bucket.size, locale)}
                          </Table.Td>
                          <Table.Td style={{ textAlign: 'right' }}>
                            {formatNumber(bucket.objects, locale)}
                          </Table.Td>
                        </>
                      )}
                      <Table.Td>
                        <time dateTime={bucket.created}>
                          {new Date(bucket.created).toLocaleDateString(locale)}
                        </time>
                      </Table.Td>
                      <Table.Td style={{ textAlign: 'right' }}>
                        <BucketActions bucket={bucket} onDelete={setDeleting} />
                      </Table.Td>
                    </Table.Tr>
                  ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
        {query.isSuccess && rows.length === 0 ? (
          <Stack align="center" gap="xs" p="xl">
            <Text fw={500}>
              {search.trim() === '' ? t('buckets.empty') : t('buckets.noMatch', { search })}
            </Text>
            {search.trim() === '' && canCreate ? (
              <Text size="sm" c="dimmed">
                {t('buckets.emptyHint')}
              </Text>
            ) : null}
          </Stack>
        ) : null}
      </Paper>

      <CreateBucketDialog
        opened={creating}
        onClose={() => {
          setCreating(false);
        }}
      />
      <DeleteBucketDialog
        bucket={deleting}
        onClose={() => {
          setDeleting(null);
        }}
      />
    </Stack>
  );
}
