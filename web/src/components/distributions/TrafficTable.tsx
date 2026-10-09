import { Table, Text } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { formatBytes, formatNumber } from '@/lib/format';
import type { HostTraffic } from '@/services';

/** D-DST-08: the traffic of every alias of a distribution. */
export function TrafficTable({ hosts }: { hosts: readonly HostTraffic[] }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  if (hosts.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {t('distributions.traffic.none')}
      </Text>
    );
  }
  return (
    <Table aria-label={t('distributions.traffic.title')} verticalSpacing={4}>
      <Table.Thead>
        <Table.Tr>
          <Table.Th>{t('distributions.traffic.host')}</Table.Th>
          <Table.Th ta="right">{t('distributions.traffic.requests')}</Table.Th>
          <Table.Th ta="right">{t('distributions.traffic.bytes')}</Table.Th>
          <Table.Th ta="right">{t('distributions.traffic.hitRatio')}</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {hosts.map((h) => (
          <Table.Tr key={h.host}>
            <Table.Td>
              <Text size="sm" ff="monospace">
                {h.host}
              </Text>
            </Table.Td>
            <Table.Td ta="right">{formatNumber(h.requests, locale)}</Table.Td>
            <Table.Td ta="right">{formatBytes(h.bytes, locale)}</Table.Td>
            <Table.Td ta="right">{`${(h.hitRatio * 100).toFixed(0)}%`}</Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}
