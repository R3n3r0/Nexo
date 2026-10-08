import {
  ActionIcon,
  Badge,
  Button,
  Group,
  Paper,
  Select,
  Skeleton,
  Stack,
  Table,
  Text,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import { IconEdit, IconPlus, IconTrash } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router';

import { useSession } from '@/auth/SessionProvider';
import { usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog, ErrorAlert, PageHeader, StatusBadge } from '@/components/iam/common';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { useServices, type AccessKey, type CreatedAccessKey } from '@/services';

import { AccessKeyFormModal } from './AccessKeyFormModal';
import { SecretRevealModal } from './SecretRevealModal';

/** Value of the owner filter meaning "every user" (admins only). */
const allUsers = '*';

/**
 * Access keys page (D-IAM-04): the caller's keys, or those of a user (or
 * of everybody) for administrators; create with a secret shown once,
 * edit expiration, status and inline policy, delete.
 */
export function AccessKeysPage() {
  const { t, i18n } = useTranslation();
  const services = useServices();
  const { session } = useSession();
  const { can } = usePermissions();
  const [params, setParams] = useSearchParams();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<AccessKey | null>(null);
  const [deleting, setDeleting] = useState<AccessKey | null>(null);
  const [created, setCreated] = useState<CreatedAccessKey | null>(null);

  const isAdmin = can('admin:ListServiceAccounts');
  const me = session?.accessKey ?? '';
  const ownerParam = params.get('user') ?? '';
  // Admins see everybody's keys by default (no user filter) or those of
  // the chosen user; other users always see their own keys.
  const owner = isAdmin ? (ownerParam === '' ? allUsers : ownerParam) : me;
  const listFilter = isAdmin && owner !== allUsers ? { user: owner } : {};

  const keys = useQuery({
    queryKey: iamKeys.accessKeyList(listFilter),
    queryFn: () => services.accessKeys.list(listFilter),
  });
  const users = useQuery({
    queryKey: iamKeys.users,
    queryFn: () => services.users.list(),
    enabled: isAdmin && can('admin:ListUsers'),
  });
  const remove = useInvalidatingMutation({
    mutationFn: (key: AccessKey) => services.accessKeys.remove(key.accessKey),
    invalidate: [iamKeys.accessKeys],
    onSuccess: () => {
      setDeleting(null);
    },
  });

  const userNames = (users.data ?? []).map((u) => u.accessKey);
  const ownerOptions = [
    { value: allUsers, label: t('accessKeys.filter.all') },
    { value: me, label: t('accessKeys.filter.mine') },
    ...userNames.filter((u) => u !== me).map((u) => ({ value: u, label: u })),
  ];
  const formatDate = (iso: string) =>
    new Date(iso).toLocaleString(i18n.resolvedLanguage, {
      dateStyle: 'medium',
      timeStyle: 'short',
    });
  // Self-service: every signed-in user manages its own access keys.
  const canCreate = session !== null;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('accessKeys.title')}
        description={t('accessKeys.subtitle')}
        actions={
          canCreate ? (
            <Button
              leftSection={<IconPlus size={16} />}
              onClick={() => {
                setCreating(true);
              }}
            >
              {t('accessKeys.create')}
            </Button>
          ) : null
        }
      />
      {isAdmin ? (
        <Select
          label={t('accessKeys.filter.label')}
          data={ownerOptions}
          value={owner}
          onChange={(value) => {
            const next = new URLSearchParams(params);
            if (value === null || value === allUsers) next.delete('user');
            else next.set('user', value);
            setParams(next, { replace: true });
          }}
          searchable
          allowDeselect={false}
          maw={320}
        />
      ) : null}
      {keys.isError ? (
        <ErrorAlert
          error={keys.error}
          title={t('accessKeys.loadError')}
          onRetry={() => {
            void keys.refetch();
          }}
        />
      ) : null}
      <Paper withBorder radius="md">
        <Table.ScrollContainer minWidth={720}>
          <Table striped highlightOnHover verticalSpacing="sm" aria-label={t('accessKeys.title')}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('accessKeys.fields.accessKey')}</Table.Th>
                <Table.Th>{t('accessKeys.fields.name')}</Table.Th>
                <Table.Th>{t('accessKeys.fields.owner')}</Table.Th>
                <Table.Th>{t('common.status')}</Table.Th>
                <Table.Th>{t('accessKeys.fields.expiration')}</Table.Th>
                <Table.Th>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {keys.isPending ? (
                <Table.Tr>
                  <Table.Td colSpan={6}>
                    <Skeleton h={20} />
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {keys.data !== undefined && keys.data.length === 0 ? (
                <Table.Tr>
                  <Table.Td colSpan={6}>
                    <Text c="dimmed" ta="center" py="md">
                      {t('accessKeys.empty')}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {(keys.data ?? []).map((key) => {
                const expired =
                  key.expiration !== undefined && Date.parse(key.expiration) <= Date.now();
                return (
                  <Table.Tr key={key.accessKey}>
                    <Table.Td>
                      <Text ff="monospace" size="sm">
                        {key.accessKey}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Stack gap={0}>
                        <Group gap={4}>
                          <Text size="sm">{key.name !== '' ? key.name : '—'}</Text>
                          {key.policy !== undefined ? (
                            <Badge size="xs" variant="light">
                              {t('accessKeys.inline')}
                            </Badge>
                          ) : null}
                        </Group>
                        {key.description !== '' ? (
                          <Text size="xs" c="dimmed">
                            {key.description}
                          </Text>
                        ) : null}
                      </Stack>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{key.parentUser}</Text>
                    </Table.Td>
                    <Table.Td>
                      <StatusBadge status={key.status} />
                    </Table.Td>
                    <Table.Td>
                      {key.expiration === undefined ? (
                        <Text size="sm" c="dimmed">
                          {t('accessKeys.never')}
                        </Text>
                      ) : (
                        <Group gap={4}>
                          <Text size="sm">{formatDate(key.expiration)}</Text>
                          {expired ? (
                            <Badge size="xs" color="red" variant="light">
                              {t('accessKeys.expired')}
                            </Badge>
                          ) : null}
                        </Group>
                      )}
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="nowrap">
                        <Tooltip label={t('accessKeys.edit', { key: key.accessKey })}>
                          <ActionIcon
                            variant="subtle"
                            aria-label={t('accessKeys.edit', { key: key.accessKey })}
                            onClick={() => {
                              setEditing(key);
                            }}
                          >
                            <IconEdit size={16} />
                          </ActionIcon>
                        </Tooltip>
                        <Tooltip label={t('accessKeys.delete.action', { key: key.accessKey })}>
                          <ActionIcon
                            variant="subtle"
                            color="red"
                            aria-label={t('accessKeys.delete.action', { key: key.accessKey })}
                            onClick={() => {
                              remove.reset();
                              setDeleting(key);
                            }}
                          >
                            <IconTrash size={16} />
                          </ActionIcon>
                        </Tooltip>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                );
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Paper>

      <AccessKeyFormModal
        opened={creating}
        onClose={() => {
          setCreating(false);
        }}
        userOptions={isAdmin && can('admin:CreateServiceAccount') ? userNames : []}
        {...(isAdmin && owner !== allUsers ? { defaultUser: owner } : {})}
        onCreated={setCreated}
      />
      <AccessKeyFormModal
        opened={editing !== null}
        onClose={() => {
          setEditing(null);
        }}
        {...(editing !== null ? { accessKey: editing } : {})}
      />
      <SecretRevealModal
        created={created}
        onClose={() => {
          setCreated(null);
        }}
      />
      <ConfirmDialog
        opened={deleting !== null}
        title={t('accessKeys.delete.title')}
        message={t('accessKeys.delete.message', { key: deleting?.accessKey ?? '' })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setDeleting(null);
        }}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting);
        }}
      />
    </Stack>
  );
}
