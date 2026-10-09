import { Paper, Progress, Stack, Table, Text, Title } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { formatBytes, formatNumber } from '@/lib/format';
import type { ServerInfo } from '@/services';

/**
 * The healing the cluster does by itself: the background queue (MRF) of
 * every node, the objects written or read without every drive, and the
 * drives being repopulated after a replacement, with their progress.
 */
export function BackgroundHealing({ info }: { info: ServerInfo }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const queues = info.nodes.filter((n) => n.healQueue !== undefined);
  const healing = info.nodes.flatMap((node) =>
    node.disks.filter((d) => d.healing).map((disk) => ({ node, disk })),
  );
  const title = t('healing.background.title');
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="md">
        <Stack gap={2}>
          <Title order={4}>{title}</Title>
          <Text size="sm" c="dimmed">
            {t('healing.background.hint')}
          </Text>
        </Stack>
        {queues.length > 0 ? (
          <Table.ScrollContainer minWidth={520}>
            <Table aria-label={t('healing.background.queues')}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('cluster.nodes.node')}</Table.Th>
                  <Table.Th ta="right">{t('healing.background.pending')}</Table.Th>
                  <Table.Th ta="right">{t('healing.background.healed')}</Table.Th>
                  <Table.Th ta="right">{t('healing.background.retried')}</Table.Th>
                  <Table.Th ta="right">{t('healing.background.dropped')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {queues.map((node) => (
                  <Table.Tr key={node.endpoint}>
                    <Table.Td style={{ overflowWrap: 'anywhere' }}>{node.endpoint}</Table.Td>
                    <Table.Td ta="right">
                      {formatNumber(node.healQueue?.pending ?? 0, locale)}
                    </Table.Td>
                    <Table.Td ta="right">
                      {formatNumber(node.healQueue?.healed ?? 0, locale)}
                    </Table.Td>
                    <Table.Td ta="right">
                      {formatNumber(node.healQueue?.retried ?? 0, locale)}
                    </Table.Td>
                    <Table.Td ta="right">
                      {formatNumber(node.healQueue?.dropped ?? 0, locale)}
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        ) : null}
        <Stack gap="xs">
          <Text fw={600} size="sm">
            {t('healing.background.drives')}
          </Text>
          {healing.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t('healing.background.noDrives')}
            </Text>
          ) : (
            healing.map(({ node, disk }) => {
              const p = disk.healingProgress;
              return (
                <Stack key={`${node.endpoint}:${disk.endpoint}`} gap={2}>
                  <Text size="sm">
                    <Text span ff="monospace">
                      {disk.path}
                    </Text>{' '}
                    <Text span c="dimmed">
                      {t('healing.background.onNode', { node: node.endpoint })}
                    </Text>
                  </Text>
                  <Progress
                    value={100}
                    animated
                    striped
                    size="sm"
                    color="yellow"
                    aria-label={t('healing.background.driveProgress', { path: disk.path })}
                  />
                  {p ? (
                    <Text size="xs" c="dimmed">
                      {t('healing.background.progress', {
                        objects: formatNumber(p.objectsHealed, locale),
                        bytes: formatBytes(p.bytesDone, locale),
                        failed: formatNumber(p.objectsFailed, locale),
                        buckets: formatNumber(p.healedBuckets, locale),
                      })}
                      {p.bucket !== undefined && p.bucket !== ''
                        ? ` · ${[p.bucket, p.object].filter(Boolean).join('/')}`
                        : ''}
                    </Text>
                  ) : null}
                </Stack>
              );
            })
          )}
        </Stack>
      </Stack>
    </Paper>
  );
}
