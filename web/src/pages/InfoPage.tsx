import {
  ActionIcon,
  Alert,
  Badge,
  CopyButton,
  Group,
  Paper,
  Skeleton,
  Stack,
  Table,
  Text,
  Title,
  Tooltip,
} from '@mantine/core';
import { IconCheck, IconCopy } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { formatDuration } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { serverInfoQueryKey } from '@/lib/serverInfo';
import { useServices } from '@/services';

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Table.Tr>
      <Table.Th w={200}>{label}</Table.Th>
      <Table.Td style={{ overflowWrap: 'anywhere' }}>{children}</Table.Td>
    </Table.Tr>
  );
}

/** D-CFG-02: version, build, deployment ID and mode of the server. */
export function InfoPage() {
  const { t, i18n } = useTranslation();
  const { info } = useServices();
  const query = useQuery({ queryKey: serverInfoQueryKey, queryFn: () => info.serverInfo() });
  const data = query.data;
  const locale = i18n.resolvedLanguage;

  return (
    <Stack gap="lg">
      <Stack gap={2}>
        <Title order={2}>{t('infoPage.title')}</Title>
        <Text c="dimmed" size="sm">
          {t('infoPage.subtitle')}
        </Text>
      </Stack>
      {query.isError ? (
        <Alert color="red" variant="light" role="alert">
          {problemMessage(t, query.error)}
        </Alert>
      ) : null}
      <Paper withBorder radius="md" p="md">
        {query.isPending ? (
          <Stack gap="sm">
            {[0, 1, 2, 3, 4].map((i) => (
              <Skeleton key={i} height={18} />
            ))}
          </Stack>
        ) : data ? (
          <Table variant="vertical" layout="fixed" aria-label={t('infoPage.title')}>
            <Table.Tbody>
              <Row label={t('infoPage.version')}>{data.version}</Row>
              <Row label={t('infoPage.commit')}>
                <Text size="sm" ff="monospace">
                  {data.commit}
                </Text>
              </Row>
              <Row label={t('infoPage.buildDate')}>
                {Number.isNaN(Date.parse(data.buildDate))
                  ? data.buildDate
                  : new Date(data.buildDate).toLocaleString(locale)}
              </Row>
              <Row label={t('infoPage.deploymentId')}>
                <Group gap="xs" wrap="nowrap">
                  <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                    {data.deploymentId}
                  </Text>
                  <CopyButton value={data.deploymentId}>
                    {({ copied, copy }) => (
                      <Tooltip label={copied ? t('infoPage.copied') : t('infoPage.copy')}>
                        <ActionIcon
                          variant="subtle"
                          aria-label={t('infoPage.copyDeploymentId')}
                          onClick={copy}
                          color={copied ? 'green' : undefined}
                        >
                          {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                        </ActionIcon>
                      </Tooltip>
                    )}
                  </CopyButton>
                </Group>
              </Row>
              <Row label={t('infoPage.mode')}>
                <Badge variant="light">{t(`modes.${data.mode}`)}</Badge>
              </Row>
              <Row label={t('infoPage.region')}>{data.region}</Row>
              <Row label={t('infoPage.startedAt')}>
                {new Date(data.startedAt).toLocaleString(locale)}
              </Row>
              <Row label={t('infoPage.uptime')}>
                {formatDuration(data.uptimeSeconds, (unit, count) =>
                  t(`duration.${unit}`, { count }),
                )}
              </Row>
              <Row label={t('infoPage.topology')}>
                {t('infoPage.topologySummary', {
                  pools: data.pools.length,
                  nodes: data.nodes.length,
                  drives: data.nodes.reduce((sum, n) => sum + n.disks.length, 0),
                })}
              </Row>
            </Table.Tbody>
          </Table>
        ) : null}
      </Paper>
      {data && data.pools.length > 0 ? (
        <Paper withBorder radius="md" p="md">
          <Title order={4} mb="sm">
            {t('infoPage.pools')}
          </Title>
          <Table.ScrollContainer minWidth={420}>
            <Table aria-label={t('infoPage.pools')}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('infoPage.pool')}</Table.Th>
                  <Table.Th>{t('infoPage.sets')}</Table.Th>
                  <Table.Th>{t('infoPage.drivesPerSet')}</Table.Th>
                  <Table.Th>{t('infoPage.parity')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.pools.map((pool) => (
                  <Table.Tr key={pool.index}>
                    <Table.Td>{pool.index}</Table.Td>
                    <Table.Td>{pool.sets}</Table.Td>
                    <Table.Td>{pool.drivesPerSet}</Table.Td>
                    <Table.Td>
                      {pool.parity === 0 ? t('infoPage.noParity') : `EC:${String(pool.parity)}`}
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Paper>
      ) : null}
    </Stack>
  );
}
