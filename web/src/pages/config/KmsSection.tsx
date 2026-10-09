import {
  Badge,
  Button,
  Group,
  Paper,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconKey } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { ErrorAlert } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices } from '@/services';

import { KmsStatusTable } from './KmsStatusTable';

const keyPattern = /^[A-Za-z0-9_.-]{1,80}$/;

/** D-CFG-01: the KMS of the server-side encryption and its keys. */
export function KmsSection() {
  const { t, i18n } = useTranslation();
  const { kms } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const [name, setName] = useState('');
  const status = useQuery({
    queryKey: storageKeys.kmsStatus,
    queryFn: () => kms.status(),
    retry: false,
  });
  const configured = status.data?.configured === true;
  const keys = useQuery({
    queryKey: storageKeys.kmsKeys,
    queryFn: () => kms.keys(),
    enabled: configured,
    retry: false,
  });
  const create = useMutation({
    mutationFn: (id: string) => kms.createKey(id),
    onSuccess: async (key) => {
      setName('');
      await queryClient.invalidateQueries({ queryKey: storageKeys.kmsKeys });
      notifications.show({ color: 'green', message: t('kms.created', { key: key.id }) });
    },
  });
  const invalid = name.trim() !== '' && !keyPattern.test(name.trim());
  const locale = i18n.resolvedLanguage;

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('kms.title')}>
      <Stack gap="sm">
        <Stack gap={2}>
          <Title order={3}>{t('kms.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('kms.description')}
          </Text>
        </Stack>
        {status.isPending ? (
          <Skeleton height={60} />
        ) : status.isError ? (
          isApiError(status.error) && status.error.status === 501 ? (
            <Text size="sm" c="dimmed">
              {t('kms.notConfigured')}
            </Text>
          ) : (
            <ErrorAlert error={status.error} />
          )
        ) : (
          <>
            <KmsStatusTable status={status.data} />
            {configured ? (
              <Stack gap="xs">
                <Title order={5}>{t('kms.keys')}</Title>
                {keys.isPending ? (
                  <Skeleton height={40} />
                ) : keys.isError ? (
                  <ErrorAlert error={keys.error} />
                ) : keys.data.length === 0 ? (
                  <Text size="sm" c="dimmed">
                    {t('kms.noKeys')}
                  </Text>
                ) : (
                  <Table aria-label={t('kms.keys')} verticalSpacing={4}>
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t('kms.keyId')}</Table.Th>
                        <Table.Th>{t('common.created')}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {keys.data.map((key) => (
                        <Table.Tr key={key.id}>
                          <Table.Td>
                            <Group gap="xs">
                              <Text size="sm" ff="monospace">
                                {key.id}
                              </Text>
                              {key.default === true ? (
                                <Badge size="xs" variant="light">
                                  {t('kms.default')}
                                </Badge>
                              ) : null}
                            </Group>
                          </Table.Td>
                          <Table.Td>
                            {key.createdAt !== undefined
                              ? new Date(key.createdAt).toLocaleString(locale)
                              : '—'}
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                )}
                <form
                  noValidate
                  onSubmit={(event) => {
                    event.preventDefault();
                    if (!invalid && name.trim() !== '') create.mutate(name.trim());
                  }}
                >
                  <Group align="flex-end" gap="xs">
                    <TextInput
                      label={t('kms.newKey')}
                      description={t('kms.newKeyHint')}
                      value={name}
                      error={invalid ? t('kms.keyInvalid') : undefined}
                      onChange={(event) => {
                        setName(event.currentTarget.value);
                      }}
                      w={260}
                    />
                    <PermissionGuard
                      allowed={perms.can(StorageActions.kmsCreateKey)}
                      action={StorageActions.kmsCreateKey}
                    >
                      {(disabled) => (
                        <Button
                          type="submit"
                          leftSection={<IconKey size={16} />}
                          disabled={disabled || invalid || name.trim() === ''}
                          loading={create.isPending}
                        >
                          {t('kms.createKey')}
                        </Button>
                      )}
                    </PermissionGuard>
                  </Group>
                </form>
                {create.isError ? <ErrorAlert error={create.error} /> : null}
              </Stack>
            ) : null}
          </>
        )}
      </Stack>
    </Paper>
  );
}
