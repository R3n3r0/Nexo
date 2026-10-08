import {
  Badge,
  Group,
  Loader,
  Paper,
  SimpleGrid,
  Stack,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { useDebouncedValue } from '@mantine/hooks';
import { IconCheck, IconX } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { iamKeys } from '@/lib/iamQueries';
import { useServices, type Group as IamGroup, type User } from '@/services';

import { ErrorAlert } from './common';

export interface EffectivePermissionsProps {
  /** The user whose permissions are shown. */
  user: User;
  /** Groups, to show the policies inherited from each one. */
  groups?: readonly IamGroup[];
}

/**
 * "Effective permissions" of a user (D-POL-04): where the policies come
 * from (direct and through groups) and the catalog actions the engine
 * allows or denies, globally or on a bucket/prefix (which also applies the
 * bucket policy).
 */
export function EffectivePermissions({ user, groups = [] }: EffectivePermissionsProps) {
  const { t } = useTranslation();
  const { permissions } = useServices();
  const [bucket, setBucket] = useState('');
  const [prefix, setPrefix] = useState('');
  const [filter, setFilter] = useState('');
  const [debouncedBucket] = useDebouncedValue(bucket.trim(), 300);
  const [debouncedPrefix] = useDebouncedValue(prefix, 300);
  const query = useMemo(
    () => ({
      user: user.accessKey,
      ...(debouncedBucket !== '' ? { bucket: debouncedBucket } : {}),
      ...(debouncedBucket !== '' && debouncedPrefix !== '' ? { prefix: debouncedPrefix } : {}),
    }),
    [user.accessKey, debouncedBucket, debouncedPrefix],
  );
  const result = useQuery({
    queryKey: iamKeys.permissionsOf(query),
    queryFn: () => permissions.get(query),
  });

  const inherited = useMemo(
    () =>
      user.groups.map((name) => {
        const group = groups.find((g) => g.name === name);
        return {
          name,
          enabled: group?.status !== 'disabled',
          policies: group?.policyNames ?? [],
        };
      }),
    [user.groups, groups],
  );

  const needle = filter.trim().toLowerCase();
  const matches = (action: string) => needle === '' || action.toLowerCase().includes(needle);
  const allowed = (result.data?.allowed ?? []).filter(matches);
  const denied = (result.data?.denied ?? []).filter(matches);

  return (
    <Stack gap="md">
      <Paper withBorder p="sm" radius="md">
        <Stack gap="xs">
          <Title order={4}>{t('permissions.sources')}</Title>
          <Group gap="xs">
            <Text size="sm" fw={500}>
              {t('permissions.direct')}
            </Text>
            {user.policyNames.length === 0 ? (
              <Text size="sm" c="dimmed">
                {t('permissions.none')}
              </Text>
            ) : (
              user.policyNames.map((p) => (
                <Badge key={p} variant="light">
                  {p}
                </Badge>
              ))
            )}
          </Group>
          {inherited.map((g) => (
            <Group gap="xs" key={g.name}>
              <Text size="sm" fw={500}>
                {t('permissions.viaGroup', { group: g.name })}
              </Text>
              {!g.enabled ? (
                <Badge color="gray" variant="outline">
                  {t('iam.status.disabled')}
                </Badge>
              ) : null}
              {g.policies.map((p) => (
                <Badge key={p} variant="light" color="grape">
                  {p}
                </Badge>
              ))}
            </Group>
          ))}
        </Stack>
      </Paper>

      <Group grow align="flex-end" wrap="wrap">
        <TextInput
          label={t('permissions.bucket')}
          description={t('permissions.bucketHint')}
          value={bucket}
          onChange={(event) => {
            setBucket(event.currentTarget.value);
          }}
        />
        <TextInput
          label={t('permissions.prefix')}
          value={prefix}
          disabled={bucket.trim() === ''}
          onChange={(event) => {
            setPrefix(event.currentTarget.value);
          }}
        />
        <TextInput
          label={t('permissions.filter')}
          value={filter}
          onChange={(event) => {
            setFilter(event.currentTarget.value);
          }}
        />
      </Group>

      {result.isError ? (
        <ErrorAlert
          error={result.error}
          title={t('permissions.loadError')}
          onRetry={() => {
            void result.refetch();
          }}
        />
      ) : null}
      {result.isPending ? <Loader size="sm" aria-label={t('common.loading')} /> : null}
      {result.data ? (
        <>
          {result.data.isRoot ? (
            <Badge color="orange" variant="light">
              {t('permissions.root')}
            </Badge>
          ) : null}
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <ActionList
              title={t('permissions.allowed', { count: allowed.length })}
              actions={allowed}
              allowed
            />
            <ActionList
              title={t('permissions.denied', { count: denied.length })}
              actions={denied}
              allowed={false}
            />
          </SimpleGrid>
        </>
      ) : null}
    </Stack>
  );
}

function ActionList({
  title,
  actions,
  allowed,
}: {
  title: string;
  actions: string[];
  allowed: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Paper withBorder p="sm" radius="md" component="section" aria-label={title}>
      <Title order={5} mb="xs">
        {title}
      </Title>
      {actions.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t('permissions.none')}
        </Text>
      ) : (
        <Stack gap={2} component="ul" style={{ listStyle: 'none', padding: 0, margin: 0 }}>
          {actions.map((action) => (
            <Group key={action} gap={6} component="li" wrap="nowrap">
              {allowed ? (
                <IconCheck size={14} color="var(--mantine-color-teal-6)" aria-hidden />
              ) : (
                <IconX size={14} color="var(--mantine-color-red-6)" aria-hidden />
              )}
              <Text size="sm" ff="monospace">
                {action}
              </Text>
            </Group>
          ))}
        </Stack>
      )}
    </Paper>
  );
}
