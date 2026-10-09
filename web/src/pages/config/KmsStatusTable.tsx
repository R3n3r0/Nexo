import { Badge, Group, Table, Text } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import type { KmsStatus } from '@/services';

/** The state of the KMS: backend, default key, servers (Info and Configuration pages). */
export function KmsStatusTable({ status }: { status: KmsStatus }) {
  const { t } = useTranslation();
  if (!status.configured) {
    return (
      <Text size="sm" c="dimmed">
        {t('kms.notConfigured')}
      </Text>
    );
  }
  return (
    <Table variant="vertical" layout="fixed" aria-label={t('kms.title')}>
      <Table.Tbody>
        <Table.Tr>
          <Table.Th w={200}>{t('kms.backend')}</Table.Th>
          <Table.Td>
            {status.backend !== undefined ? t(`kms.backends.${status.backend}`) : '—'}
            {status.version !== undefined && status.version !== '' ? ` (${status.version})` : ''}
          </Table.Td>
        </Table.Tr>
        <Table.Tr>
          <Table.Th w={200}>{t('kms.state')}</Table.Th>
          <Table.Td>
            <Group gap="xs">
              <Badge variant="light" color={status.online ? 'green' : 'red'}>
                {status.online ? t('kms.online') : t('kms.offline')}
              </Badge>
              {status.error !== undefined && status.error !== '' ? (
                <Text size="xs" c="red">
                  {status.error}
                </Text>
              ) : null}
            </Group>
          </Table.Td>
        </Table.Tr>
        <Table.Tr>
          <Table.Th w={200}>{t('kms.defaultKey')}</Table.Th>
          <Table.Td>
            <Text size="sm" ff="monospace">
              {status.defaultKeyId ?? '—'}
            </Text>
          </Table.Td>
        </Table.Tr>
        {(status.endpoints ?? []).map((e) => (
          <Table.Tr key={e.url}>
            <Table.Th w={200}>{t('kms.endpoint')}</Table.Th>
            <Table.Td>
              <Group gap="xs">
                <Text size="sm" ff="monospace">
                  {e.url}
                </Text>
                <Badge size="xs" variant="light" color={e.online ? 'green' : 'red'}>
                  {e.online ? t('kms.online') : t('kms.offline')}
                </Badge>
              </Group>
            </Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}
