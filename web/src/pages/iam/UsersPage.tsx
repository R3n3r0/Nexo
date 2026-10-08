import {
  ActionIcon,
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
  VisuallyHidden,
} from '@mantine/core';
import {
  IconDots,
  IconPlus,
  IconSearch,
  IconTrash,
  IconUserCheck,
  IconUserOff,
} from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog, ErrorAlert, PageHeader, StatusBadge } from '@/components/iam/common';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { useServices, type User } from '@/services';

import { CreateUserModal } from './CreateUserModal';
import { UserDrawer } from './UserDrawer';

/** Users page (D-IAM-01): search, create, enable/disable, delete, details. */
export function UsersPage() {
  const { t, i18n } = useTranslation();
  const services = useServices();
  const { can } = usePermissions();
  const [search, setSearch] = useState('');
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<User | null>(null);

  const usersQuery = useQuery({ queryKey: iamKeys.users, queryFn: () => services.users.list() });
  const policiesQuery = useQuery({
    queryKey: iamKeys.policies,
    queryFn: () => services.policies.list(),
    enabled: can('admin:ListUserPolicies'),
  });
  const groupsQuery = useQuery({
    queryKey: iamKeys.groups,
    queryFn: () => services.groups.list(),
    enabled: can('admin:ListGroups'),
  });

  const setStatus = useInvalidatingMutation({
    mutationFn: (user: User) =>
      services.users.update(user.accessKey, {
        status: user.status === 'enabled' ? 'disabled' : 'enabled',
      }),
    invalidate: [iamKeys.users, iamKeys.permissions],
  });
  const remove = useInvalidatingMutation({
    mutationFn: (user: User) => services.users.remove(user.accessKey),
    invalidate: [iamKeys.users, iamKeys.groups, iamKeys.accessKeys],
    onSuccess: () => {
      setDeleting(null);
    },
  });

  const users = usersQuery.data;
  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    return (users ?? []).filter(
      (u) =>
        needle === '' ||
        u.accessKey.toLowerCase().includes(needle) ||
        u.policyNames.some((p) => p.toLowerCase().includes(needle)) ||
        u.groups.some((g) => g.toLowerCase().includes(needle)),
    );
  }, [users, search]);
  const selectedUser = users?.find((u) => u.accessKey === selected) ?? null;
  const policyNames = (policiesQuery.data ?? []).map((p) => p.name);
  const date = (iso: string) =>
    new Date(iso).toLocaleDateString(i18n.resolvedLanguage, { dateStyle: 'medium' });

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('users.title')}
        description={t('users.subtitle')}
        actions={
          can('admin:CreateUser') ? (
            <Button
              leftSection={<IconPlus size={16} />}
              onClick={() => {
                setCreating(true);
              }}
            >
              {t('users.create.button')}
            </Button>
          ) : null
        }
      />

      <TextInput
        label={t('common.search')}
        placeholder={t('users.searchPlaceholder')}
        leftSection={<IconSearch size={16} />}
        value={search}
        onChange={(event) => {
          setSearch(event.currentTarget.value);
        }}
        maw={360}
      />

      {usersQuery.isError ? (
        <ErrorAlert
          error={usersQuery.error}
          title={t('users.loadError')}
          onRetry={() => {
            void usersQuery.refetch();
          }}
        />
      ) : null}
      {setStatus.isError ? <ErrorAlert error={setStatus.error} /> : null}

      <Paper withBorder radius="md">
        <Table.ScrollContainer minWidth={640}>
          <Table striped highlightOnHover verticalSpacing="sm" aria-label={t('users.title')}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('users.fields.accessKey')}</Table.Th>
                <Table.Th>{t('common.status')}</Table.Th>
                <Table.Th>{t('users.fields.policies')}</Table.Th>
                <Table.Th>{t('users.fields.groups')}</Table.Th>
                <Table.Th>{t('common.created')}</Table.Th>
                <Table.Th>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {usersQuery.isPending
                ? [0, 1, 2].map((i) => (
                    <Table.Tr key={i}>
                      <Table.Td colSpan={6}>
                        <Skeleton h={20} />
                      </Table.Td>
                    </Table.Tr>
                  ))
                : null}
              {users !== undefined && filtered.length === 0 ? (
                <Table.Tr>
                  <Table.Td colSpan={6}>
                    <Text c="dimmed" ta="center" py="md">
                      {search === '' ? t('users.empty') : t('users.noMatch')}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {filtered.map((user) => (
                <Table.Tr key={user.accessKey}>
                  <Table.Td>
                    <Anchor
                      component="button"
                      type="button"
                      fw={500}
                      onClick={() => {
                        setSelected(user.accessKey);
                      }}
                    >
                      {user.accessKey}
                    </Anchor>
                  </Table.Td>
                  <Table.Td>
                    <StatusBadge status={user.status} />
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      {user.policyNames.map((p) => (
                        <Badge key={p} variant="light" size="sm">
                          {p}
                        </Badge>
                      ))}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      {user.groups.map((g) => (
                        <Badge key={g} variant="outline" size="sm" color="grape">
                          {g}
                        </Badge>
                      ))}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{date(user.createdAt)}</Text>
                  </Table.Td>
                  <Table.Td>
                    <UserActions
                      user={user}
                      canToggle={can(
                        user.status === 'enabled' ? 'admin:DisableUser' : 'admin:EnableUser',
                      )}
                      canDelete={can('admin:DeleteUser')}
                      onToggle={() => {
                        setStatus.mutate(user);
                      }}
                      onDelete={() => {
                        remove.reset();
                        setDeleting(user);
                      }}
                    />
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Paper>

      <CreateUserModal
        opened={creating}
        onClose={() => {
          setCreating(false);
        }}
        policyNames={policyNames}
        groupNames={(groupsQuery.data ?? []).map((g) => g.name)}
      />
      <UserDrawer
        user={selectedUser}
        onClose={() => {
          setSelected(null);
        }}
        policyNames={policyNames}
        groups={groupsQuery.data ?? []}
      />
      <ConfirmDialog
        opened={deleting !== null}
        title={t('users.delete.title')}
        message={t('users.delete.message', { user: deleting?.accessKey ?? '' })}
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

function UserActions({
  user,
  canToggle,
  canDelete,
  onToggle,
  onDelete,
}: {
  user: User;
  canToggle: boolean;
  canDelete: boolean;
  onToggle: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  if (!canToggle && !canDelete) {
    return null;
  }
  return (
    <Menu position="bottom-end" withinPortal>
      <Menu.Target>
        <ActionIcon variant="subtle" aria-label={t('users.actionsFor', { user: user.accessKey })}>
          <IconDots size={16} />
        </ActionIcon>
      </Menu.Target>
      <Menu.Dropdown>
        {canToggle ? (
          <Menu.Item
            leftSection={
              user.status === 'enabled' ? <IconUserOff size={16} /> : <IconUserCheck size={16} />
            }
            onClick={onToggle}
          >
            {user.status === 'enabled' ? t('users.disable') : t('users.enable')}
          </Menu.Item>
        ) : null}
        {canDelete ? (
          <Menu.Item color="red" leftSection={<IconTrash size={16} />} onClick={onDelete}>
            {t('common.delete')}
          </Menu.Item>
        ) : null}
      </Menu.Dropdown>
    </Menu>
  );
}
