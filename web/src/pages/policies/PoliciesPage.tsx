import {
  ActionIcon,
  Anchor,
  Badge,
  Button,
  Group,
  Paper,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import { IconFlask, IconLock, IconPlus, IconSearch, IconTrash } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog, ErrorAlert, PageHeader } from '@/components/iam/common';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { useServices, type Policy } from '@/services';

/** Number of statements of a policy. */
const statementCount = (policy: Policy) => policy.document.Statement.length;

/** Policies page (D-IAM-03): built-in policies are marked and cannot be deleted. */
export function PoliciesPage() {
  const { t, i18n } = useTranslation();
  const { policies } = useServices();
  const { can } = usePermissions();
  const [search, setSearch] = useState('');
  const [deleting, setDeleting] = useState<Policy | null>(null);

  const query = useQuery({ queryKey: iamKeys.policies, queryFn: () => policies.list() });
  const remove = useInvalidatingMutation({
    mutationFn: (policy: Policy) => policies.remove(policy.name),
    invalidate: [iamKeys.policies, iamKeys.users, iamKeys.groups, iamKeys.permissions],
    onSuccess: () => {
      setDeleting(null);
    },
  });

  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    return (query.data ?? []).filter((p) => needle === '' || p.name.toLowerCase().includes(needle));
  }, [query.data, search]);

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('policies.title')}
        description={t('policies.subtitle')}
        actions={
          <>
            {can('admin:SimulatePolicy') ? (
              <Button
                component={Link}
                to="/policy-simulator"
                variant="default"
                leftSection={<IconFlask size={16} />}
              >
                {t('policies.simulate')}
              </Button>
            ) : null}
            {can('admin:CreatePolicy') ? (
              <Button component={Link} to="/policies/new" leftSection={<IconPlus size={16} />}>
                {t('policies.create')}
              </Button>
            ) : null}
          </>
        }
      />
      <TextInput
        label={t('common.search')}
        placeholder={t('policies.searchPlaceholder')}
        leftSection={<IconSearch size={16} />}
        value={search}
        onChange={(event) => {
          setSearch(event.currentTarget.value);
        }}
        maw={360}
      />
      {query.isError ? (
        <ErrorAlert
          error={query.error}
          title={t('policies.loadError')}
          onRetry={() => {
            void query.refetch();
          }}
        />
      ) : null}
      <Paper withBorder radius="md">
        <Table.ScrollContainer minWidth={520}>
          <Table striped highlightOnHover verticalSpacing="sm" aria-label={t('policies.title')}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('common.name')}</Table.Th>
                <Table.Th>{t('policies.statements')}</Table.Th>
                <Table.Th>{t('policies.updated')}</Table.Th>
                <Table.Th>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {query.isPending ? (
                <Table.Tr>
                  <Table.Td colSpan={4}>
                    <Skeleton h={20} />
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {query.data !== undefined && filtered.length === 0 ? (
                <Table.Tr>
                  <Table.Td colSpan={4}>
                    <Text c="dimmed" ta="center" py="md">
                      {t('policies.empty')}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {filtered.map((policy) => (
                <Table.Tr key={policy.name}>
                  <Table.Td>
                    <Group gap="xs" wrap="nowrap">
                      <Anchor
                        component={Link}
                        to={`/policies/${encodeURIComponent(policy.name)}`}
                        fw={500}
                      >
                        {policy.name}
                      </Anchor>
                      {policy.builtIn ? (
                        <Badge
                          size="sm"
                          variant="light"
                          color="gray"
                          leftSection={<IconLock size={12} />}
                        >
                          {t('policies.builtIn')}
                        </Badge>
                      ) : null}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{statementCount(policy)}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">
                      {new Date(policy.updatedAt).toLocaleDateString(i18n.resolvedLanguage, {
                        dateStyle: 'medium',
                      })}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    {!policy.builtIn && can('admin:DeletePolicy') ? (
                      <Tooltip label={t('policies.delete.action', { policy: policy.name })}>
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          aria-label={t('policies.delete.action', { policy: policy.name })}
                          onClick={() => {
                            remove.reset();
                            setDeleting(policy);
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
      <ConfirmDialog
        opened={deleting !== null}
        title={t('policies.delete.title')}
        message={t('policies.delete.message', { policy: deleting?.name ?? '' })}
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
