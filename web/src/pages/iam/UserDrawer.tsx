import { Button, Drawer, Group, MultiSelect, Stack, Switch, Tabs, Text } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { usePermissions } from '@/auth/usePermissions';
import { ErrorAlert, StatusBadge } from '@/components/iam/common';
import { EffectivePermissions } from '@/components/iam/EffectivePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { useServices, type Group as IamGroup, type User } from '@/services';

export interface UserDrawerProps {
  user: User | null;
  onClose: () => void;
  policyNames: string[];
  groups: IamGroup[];
}

/** Details of a user: status, policies, groups and effective permissions. */
export function UserDrawer({ user, onClose, policyNames, groups }: UserDrawerProps) {
  const { t } = useTranslation();
  return (
    <Drawer
      opened={user !== null}
      onClose={onClose}
      position="right"
      size="xl"
      title={user ? t('users.drawer.title', { user: user.accessKey }) : ''}
    >
      {user ? (
        <Tabs defaultValue="settings" keepMounted={false}>
          <Tabs.List>
            <Tabs.Tab value="settings">{t('users.drawer.settings')}</Tabs.Tab>
            <Tabs.Tab value="permissions">{t('users.drawer.permissions')}</Tabs.Tab>
          </Tabs.List>
          <Tabs.Panel value="settings" pt="md">
            <UserSettings
              key={`${user.accessKey}:${user.updatedAt}`}
              user={user}
              policyNames={policyNames}
              groupNames={groups.map((g) => g.name)}
            />
          </Tabs.Panel>
          <Tabs.Panel value="permissions" pt="md">
            <EffectivePermissions user={user} groups={groups} />
          </Tabs.Panel>
        </Tabs>
      ) : null}
    </Drawer>
  );
}

function sameSet(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((x) => b.includes(x));
}

function UserSettings({
  user,
  policyNames,
  groupNames,
}: {
  user: User;
  policyNames: string[];
  groupNames: string[];
}) {
  const { t } = useTranslation();
  const { users } = useServices();
  const { can } = usePermissions();
  const [enabled, setEnabled] = useState(user.status === 'enabled');
  const [policies, setPolicies] = useState(user.policyNames);
  const [groups, setGroups] = useState(user.groups);

  const mutation = useInvalidatingMutation({
    mutationFn: () =>
      users.update(user.accessKey, {
        ...(enabled !== (user.status === 'enabled')
          ? { status: enabled ? ('enabled' as const) : ('disabled' as const) }
          : {}),
        ...(!sameSet(policies, user.policyNames) ? { policyNames: policies } : {}),
        ...(!sameSet(groups, user.groups) ? { groups } : {}),
      }),
    invalidate: [iamKeys.users, iamKeys.groups, iamKeys.policies, iamKeys.permissions],
    onSuccess: () => {
      notifications.show({ color: 'teal', message: t('users.saved', { user: user.accessKey }) });
    },
  });

  const dirty =
    enabled !== (user.status === 'enabled') ||
    !sameSet(policies, user.policyNames) ||
    !sameSet(groups, user.groups);
  const statusAction = enabled ? 'admin:DisableUser' : 'admin:EnableUser';

  return (
    <Stack gap="md">
      {mutation.isError ? <ErrorAlert error={mutation.error} /> : null}
      <Group gap="sm">
        <Text size="sm" fw={500}>
          {t('common.status')}
        </Text>
        <StatusBadge status={user.status} />
      </Group>
      <PermissionGuard allowed={can(statusAction)} action={statusAction}>
        {(disabled) => (
          <Switch
            label={t('users.fields.enabled')}
            checked={enabled}
            disabled={disabled}
            onChange={(event) => {
              setEnabled(event.currentTarget.checked);
            }}
          />
        )}
      </PermissionGuard>
      <PermissionGuard
        allowed={can('admin:AttachUserOrGroupPolicy')}
        action="admin:AttachUserOrGroupPolicy"
      >
        {(disabled) => (
          <MultiSelect
            label={t('users.fields.policies')}
            data={[...new Set([...policyNames, ...policies])]}
            value={policies}
            onChange={setPolicies}
            searchable
            clearable
            disabled={disabled}
          />
        )}
      </PermissionGuard>
      <PermissionGuard allowed={can('admin:AddUserToGroup')} action="admin:AddUserToGroup">
        {(disabled) => (
          <MultiSelect
            label={t('users.fields.groups')}
            data={[...new Set([...groupNames, ...groups])]}
            value={groups}
            onChange={setGroups}
            searchable
            clearable
            disabled={disabled}
          />
        )}
      </PermissionGuard>
      <Group justify="space-between">
        <Button
          component={Link}
          to={`/access-keys?user=${encodeURIComponent(user.accessKey)}`}
          variant="subtle"
        >
          {t('users.drawer.accessKeys')}
        </Button>
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
