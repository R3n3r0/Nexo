import {
  Alert,
  Anchor,
  Badge,
  Button,
  Group,
  Skeleton,
  Stack,
  Tabs,
  Text,
  Title,
} from '@mantine/core';
import {
  IconAlertCircle,
  IconFolder,
  IconLock,
  IconSettings,
  IconTrash,
} from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, Outlet, useLocation, useNavigate, useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { formatBytes, formatNumber } from '@/lib/format';
import { browsePath } from '@/lib/objectPath';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices } from '@/services';

import { DeleteBucketDialog } from './DeleteBucketDialog';

type BucketTab = 'browse' | 'settings' | 'access';

/** The tab of a bucket route: /buckets/:bucket/{browse/*,settings,access}. */
function tabOf(pathname: string, bucket: string): BucketTab {
  const base = `/buckets/${encodeURIComponent(bucket)}/`;
  const rest = pathname.startsWith(base) ? pathname.slice(base.length) : '';
  if (rest.startsWith('settings')) return 'settings';
  if (rest.startsWith('access')) return 'access';
  return 'browse';
}

/** Header and tabs of a bucket (objects, settings, access); the tab is a child route. */
export function BucketDetailPage() {
  const { t, i18n } = useTranslation();
  const { bucket: name = '' } = useParams();
  const location = useLocation();
  const navigate = useNavigate();
  const { buckets } = useServices();
  const perms = usePermissions({ bucket: name });
  const [deleting, setDeleting] = useState(false);
  const query = useQuery({ queryKey: storageKeys.bucket(name), queryFn: () => buckets.get(name) });
  const locale = i18n.resolvedLanguage;
  const tab = tabOf(location.pathname, name);

  // A 403 does not close the page: a requester that may use part of the
  // bucket (a home directory) still browses the folders it may list.
  const forbidden = query.isError && isApiError(query.error) && query.error.isForbidden;
  if (query.isError && !forbidden) {
    const missing = isApiError(query.error) && query.error.isNotFound;
    return (
      <Stack gap="md">
        <Title order={2}>{name}</Title>
        <Alert color="red" variant="light" icon={<IconAlertCircle size={18} />} role="alert">
          <Stack gap="xs" align="flex-start">
            <Text>{missing ? t('bucket.notFound', { name }) : t('bucket.loadError')}</Text>
            {missing ? (
              <Anchor component={Link} to="/buckets">
                {t('bucket.backToList')}
              </Anchor>
            ) : (
              <Button
                size="xs"
                variant="default"
                onClick={() => {
                  void query.refetch();
                }}
              >
                {t('common.retry')}
              </Button>
            )}
          </Stack>
        </Alert>
      </Stack>
    );
  }

  const bucket = query.data;
  return (
    <Stack gap="md">
      <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
        <Stack gap={4} style={{ minWidth: 0 }}>
          <Anchor component={Link} to="/buckets" size="sm">
            {t('nav.buckets')}
          </Anchor>
          <Group gap="xs" wrap="wrap">
            <Title order={2} style={{ overflowWrap: 'anywhere' }}>
              {name}
            </Title>
            {bucket?.public === true ? (
              <Badge color="orange" variant="light">
                {t('buckets.public')}
              </Badge>
            ) : null}
            {bucket?.versioning === 'enabled' ? (
              <Badge variant="outline" color="gray">
                {t('buckets.versioned')}
              </Badge>
            ) : null}
            {bucket?.objectLockEnabled === true ? (
              <Badge variant="outline" color="gray" leftSection={<IconLock size={12} />}>
                {t('buckets.locked')}
              </Badge>
            ) : null}
          </Group>
          {bucket && bucket.partial !== true ? (
            <Text size="sm" c="dimmed">
              {t('bucket.summary', {
                size: formatBytes(bucket.size, locale),
                objects: formatNumber(bucket.objects, locale),
              })}
            </Text>
          ) : bucket || forbidden ? (
            <Text size="sm" c="dimmed">
              {t('bucket.limitedAccess')}
            </Text>
          ) : (
            <Skeleton height={14} width={180} />
          )}
        </Stack>
        <PermissionGuard
          allowed={!perms.ready || perms.can(StorageActions.deleteBucket)}
          action={StorageActions.deleteBucket}
        >
          {(disabled) => (
            <Button
              color="red"
              variant="light"
              leftSection={<IconTrash size={16} />}
              disabled={disabled || !bucket}
              onClick={() => {
                setDeleting(true);
              }}
            >
              {t('bucket.delete')}
            </Button>
          )}
        </PermissionGuard>
      </Group>

      <Tabs
        value={tab}
        onChange={(value) => {
          const base = `/buckets/${encodeURIComponent(name)}`;
          if (value === 'browse') void navigate(browsePath(name));
          else if (value === 'settings' || value === 'access') void navigate(`${base}/${value}`);
        }}
        keepMounted={false}
      >
        <Tabs.List>
          <Tabs.Tab value="browse" leftSection={<IconFolder size={16} />}>
            {t('bucket.tabs.objects')}
          </Tabs.Tab>
          <Tabs.Tab value="settings" leftSection={<IconSettings size={16} />}>
            {t('bucket.tabs.settings')}
          </Tabs.Tab>
          <Tabs.Tab value="access" leftSection={<IconLock size={16} />}>
            {t('bucket.tabs.access')}
          </Tabs.Tab>
        </Tabs.List>
        {/* Every tab controls a panel; the route renders in the active one. */}
        {(['browse', 'settings', 'access'] as const).map((value) => (
          <Tabs.Panel key={value} value={value} pt="md">
            {value === tab ? <Outlet /> : null}
          </Tabs.Panel>
        ))}
      </Tabs>

      <DeleteBucketDialog
        bucket={deleting && bucket ? bucket : null}
        onClose={() => {
          setDeleting(false);
        }}
        onDeleted={() => {
          void navigate('/buckets', { replace: true });
        }}
      />
    </Stack>
  );
}
