import {
  ActionIcon,
  Anchor,
  Badge,
  Button,
  Drawer,
  Group,
  Modal,
  MultiSelect,
  Paper,
  Skeleton,
  Stack,
  Switch,
  Table,
  Text,
  TextInput,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconPlus, IconSearch, IconTrash } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog, ErrorAlert, PageHeader, StatusBadge } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { useServices, type Group as IamGroup } from '@/services';

/** Groups page (D-IAM-02): CRUD, members, policies and status. */
export function GroupsPage() {
  const { t } = useTranslation();
  const services = useServices();
  const { can } = usePermissions();
  const [search, setSearch] = useState('');
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<IamGroup | null>(null);

  const groupsQuery = useQuery({ queryKey: iamKeys.groups, queryFn: () => services.groups.list() });
  const usersQuery = useQuery({
    queryKey: iamKeys.users,
    queryFn: () => services.users.list(),
    enabled: can('admin:ListUsers'),
  });
  const policiesQuery = useQuery({
    queryKey: iamKeys.policies,
    queryFn: () => services.policies.list(),
    enabled: can('admin:ListUserPolicies'),
  });
  const remove = useInvalidatingMutation({
    mutationFn: (group: IamGroup) => services.groups.remove(group.name),
    invalidate: [iamKeys.groups, iamKeys.users, iamKeys.permissions],
    onSuccess: () => {
      setDeleting(null);
    },
  });

  const groups = groupsQuery.data;
  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    return (groups ?? []).filter(
      (g) =>
        needle === '' ||
        g.name.toLowerCase().includes(needle) ||
        g.members.some((m) => m.toLowerCase().includes(needle)),
    );
  }, [groups, search]);
  const selectedGroup = groups?.find((g) => g.name === selected) ?? null;
  const userNames = (usersQuery.data ?? []).map((u) => u.accessKey);
  const policyNames = (policiesQuery.data ?? []).map((p) => p.name);

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('groups.title')}
        description={t('groups.subtitle')}
        actions={
          can('admin:AddUserToGroup') ? (
            <Button
              leftSection={<IconPlus size={16} />}
              onClick={() => {
                setCreating(true);
              }}
            >
              {t('groups.create.button')}
            </Button>
          ) : null
        }
      />
      <TextInput
        label={t('common.search')}
        placeholder={t('groups.searchPlaceholder')}
        leftSection={<IconSearch size={16} />}
        value={search}
        onChange={(event) => {
          setSearch(event.currentTarget.value);
        }}
        maw={360}
      />
      {groupsQuery.isError ? (
        <ErrorAlert
          error={groupsQuery.error}
          title={t('groups.loadError')}
          onRetry={() => {
            void groupsQuery.refetch();
          }}
        />
      ) : null}

      <Paper withBorder radius="md">
        <Table.ScrollContainer minWidth={560}>
          <Table striped highlightOnHover verticalSpacing="sm" aria-label={t('groups.title')}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('common.name')}</Table.Th>
                <Table.Th>{t('common.status')}</Table.Th>
                <Table.Th>{t('groups.fields.members')}</Table.Th>
                <Table.Th>{t('groups.fields.policies')}</Table.Th>
                <Table.Th>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {groupsQuery.isPending ? (
                <Table.Tr>
                  <Table.Td colSpan={5}>
                    <Skeleton h={20} />
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {groups !== undefined && filtered.length === 0 ? (
                <Table.Tr>
                  <Table.Td colSpan={5}>
                    <Text c="dimmed" ta="center" py="md">
                      {search === '' ? t('groups.empty') : t('groups.noMatch')}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {filtered.map((group) => (
                <Table.Tr key={group.name}>
                  <Table.Td>
                    <Anchor
                      component="button"
                      type="button"
                      fw={500}
                      onClick={() => {
                        setSelected(group.name);
                      }}
                    >
                      {group.name}
                    </Anchor>
                  </Table.Td>
                  <Table.Td>
                    <StatusBadge status={group.status} />
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">
                      {t('groups.memberCount', { count: group.members.length })}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      {group.policyNames.map((p) => (
                        <Badge key={p} variant="light" size="sm">
                          {p}
                        </Badge>
                      ))}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    {can('admin:RemoveUserFromGroup') ? (
                      <Tooltip label={t('groups.delete.action', { group: group.name })}>
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          aria-label={t('groups.delete.action', { group: group.name })}
                          onClick={() => {
                            remove.reset();
                            setDeleting(group);
                          }}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Tooltip>
                    ) : null}
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Paper>

      <CreateGroupModal
        opened={creating}
        onClose={() => {
          setCreating(false);
        }}
        userNames={userNames}
        policyNames={policyNames}
      />
      <Drawer
        opened={selectedGroup !== null}
        onClose={() => {
          setSelected(null);
        }}
        position="right"
        size="lg"
        title={selectedGroup ? t('groups.drawer.title', { group: selectedGroup.name }) : ''}
      >
        {selectedGroup ? (
          <GroupSettings
            key={`${selectedGroup.name}:${selectedGroup.updatedAt}`}
            group={selectedGroup}
            userNames={userNames}
            policyNames={policyNames}
          />
        ) : null}
      </Drawer>
      <ConfirmDialog
        opened={deleting !== null}
        title={t('groups.delete.title')}
        message={t('groups.delete.message', { group: deleting?.name ?? '' })}
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

function CreateGroupModal({
  opened,
  onClose,
  userNames,
  policyNames,
}: {
  opened: boolean;
  onClose: () => void;
  userNames: string[];
  policyNames: string[];
}) {
  const { t } = useTranslation();
  const { groups } = useServices();
  const [name, setName] = useState('');
  const [members, setMembers] = useState<string[]>([]);
  const [policies, setPolicies] = useState<string[]>([]);
  const [touched, setTouched] = useState(false);
  const mutation = useInvalidatingMutation({
    mutationFn: () => groups.create({ name: name.trim(), members, policyNames: policies }),
    invalidate: [iamKeys.groups, iamKeys.users, iamKeys.permissions],
    onSuccess: () => {
      close();
    },
  });
  const nameProblem =
    name.trim() === ''
      ? 'required'
      : name.includes(',') || name.length > 128
        ? 'invalid'
        : undefined;

  function close() {
    setName('');
    setMembers([]);
    setPolicies([]);
    setTouched(false);
    mutation.reset();
    onClose();
  }

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setTouched(true);
    if (nameProblem === undefined) mutation.mutate();
  };

  return (
    <Modal opened={opened} onClose={close} title={t('groups.create.title')}>
      <form onSubmit={submit} noValidate aria-label={t('groups.create.title')}>
        <Stack gap="md">
          {mutation.isError ? <ErrorAlert error={mutation.error} /> : null}
          <TextInput
            label={t('common.name')}
            required
            value={name}
            onChange={(event) => {
              setName(event.currentTarget.value);
            }}
            error={
              touched && nameProblem !== undefined
                ? t(`groups.validation.${nameProblem}`)
                : undefined
            }
            data-autofocus
          />
          <MultiSelect
            label={t('groups.fields.members')}
            data={userNames}
            value={members}
            onChange={setMembers}
            searchable
            clearable
          />
          <MultiSelect
            label={t('groups.fields.policies')}
            data={policyNames}
            value={policies}
            onChange={setPolicies}
            searchable
            clearable
          />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={close} disabled={mutation.isPending}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              {t('common.create')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function GroupSettings({
  group,
  userNames,
  policyNames,
}: {
  group: IamGroup;
  userNames: string[];
  policyNames: string[];
}) {
  const { t } = useTranslation();
  const { groups } = useServices();
  const { can } = usePermissions();
  const [enabled, setEnabled] = useState(group.status === 'enabled');
  const [members, setMembers] = useState(group.members);
  const [policies, setPolicies] = useState(group.policyNames);

  const addMembers = members.filter((m) => !group.members.includes(m));
  const removeMembers = group.members.filter((m) => !members.includes(m));
  const policiesChanged =
    policies.length !== group.policyNames.length ||
    policies.some((p) => !group.policyNames.includes(p));
  const statusChanged = enabled !== (group.status === 'enabled');
  const dirty = statusChanged || policiesChanged || addMembers.length + removeMembers.length > 0;

  const mutation = useInvalidatingMutation({
    mutationFn: () =>
      groups.update(group.name, {
        ...(statusChanged
          ? { status: enabled ? ('enabled' as const) : ('disabled' as const) }
          : {}),
        ...(policiesChanged ? { policyNames: policies } : {}),
        ...(addMembers.length > 0 ? { addMembers } : {}),
        ...(removeMembers.length > 0 ? { removeMembers } : {}),
      }),
    invalidate: [iamKeys.groups, iamKeys.users, iamKeys.policies, iamKeys.permissions],
    onSuccess: () => {
      notifications.show({ color: 'teal', message: t('groups.saved', { group: group.name }) });
    },
  });
  const statusAction = enabled ? 'admin:DisableGroup' : 'admin:EnableGroup';

  return (
    <Stack gap="md">
      {mutation.isError ? <ErrorAlert error={mutation.error} /> : null}
      <PermissionGuard allowed={can(statusAction)} action={statusAction}>
        {(disabled) => (
          <Switch
            label={t('groups.fields.enabled')}
            checked={enabled}
            disabled={disabled}
            onChange={(event) => {
              setEnabled(event.currentTarget.checked);
            }}
          />
        )}
      </PermissionGuard>
      <PermissionGuard allowed={can('admin:AddUserToGroup')} action="admin:AddUserToGroup">
        {(disabled) => (
          <MultiSelect
            label={t('groups.fields.members')}
            data={[...new Set([...userNames, ...members])]}
            value={members}
            onChange={setMembers}
            searchable
            clearable
            disabled={disabled}
          />
        )}
      </PermissionGuard>
      <PermissionGuard
        allowed={can('admin:AttachUserOrGroupPolicy')}
        action="admin:AttachUserOrGroupPolicy"
      >
        {(disabled) => (
          <MultiSelect
            label={t('groups.fields.policies')}
            data={[...new Set([...policyNames, ...policies])]}
            value={policies}
            onChange={setPolicies}
            searchable
            clearable
            disabled={disabled}
          />
        )}
      </PermissionGuard>
      <Group justify="flex-end">
        <Button
          onClick={() => {
            mutation.mutate();
          }}
          disabled={!dirty}
          loading={mutation.isPending}
        >
          {t('common.save')}
        </Button>
      </Group>
    </Stack>
  );
}
