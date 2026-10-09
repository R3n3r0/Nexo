import {
  Badge,
  Group,
  Paper,
  Progress,
  SimpleGrid,
  Stack,
  Table,
  Text,
  Title,
} from '@mantine/core';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import {
  healKeys,
  healRunningRefreshMs,
  healTarget,
  isRunning,
  jobStateColor,
} from '@/lib/cluster';
import { formatBytes, formatNumber } from '@/lib/format';
import { useServices, type HealDriveState, type HealItem, type HealJob } from '@/services';

const driveStateColor: Record<HealDriveState, string> = {
  ok: 'green',
  missing: 'yellow',
  corrupt: 'orange',
  offline: 'red',
};

function Counter({ label, value }: { label: string; value: string }) {
  return (
    <Stack gap={0}>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text fw={600}>{value}</Text>
    </Stack>
  );
}

function ItemResult({ item }: { item: HealItem }) {
  const { t } = useTranslation();
  if (item.error !== undefined && item.error !== '') {
    return (
      <Text size="sm" c="red">
        {item.error}
      </Text>
    );
  }
  if (item.removed === true) return <Badge color="gray">{t('healing.items.removed')}</Badge>;
  if (item.dangling === true) return <Badge color="orange">{t('healing.items.dangling')}</Badge>;
  return <Text size="sm">{t('healing.items.healedCopies', { count: item.healed })}</Text>;
}

/** The progress and the per-drive results of a healing job, polled while it runs. */
export function HealJobDetails({ id, initial }: { id: string; initial?: HealJob }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { heal } = useServices();
  const query = useQuery({
    queryKey: healKeys.job(id),
    queryFn: () => heal.get(id),
    ...(initial ? { initialData: initial } : {}),
    refetchInterval: (q) =>
      q.state.data && !isRunning(q.state.data) ? false : healRunningRefreshMs,
  });
  const job = query.data;
  const title = t('healing.details.title', { id });
  if (!job) {
    return null;
  }
  const target = healTarget(job.request);
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="md">
        <Group justify="space-between" wrap="wrap">
          <Stack gap={2}>
            <Title order={4}>{title}</Title>
            <Text size="sm" c="dimmed">
              {target === undefined ? t('healing.target.all') : target}
              {job.node !== undefined && job.node !== ''
                ? ` · ${t('healing.details.node', { node: job.node })}`
                : ''}
            </Text>
          </Stack>
          <Badge color={jobStateColor[job.state]} variant="light" size="lg">
            {t(`healing.states.${job.state}`)}
          </Badge>
        </Group>
        {isRunning(job) ? (
          <Stack gap={4}>
            <Progress value={100} animated striped aria-label={t('healing.details.progress')} />
            <Text size="xs" c="dimmed">
              {job.current !== undefined && job.current !== ''
                ? t('healing.details.current', { item: job.current })
                : t('healing.details.starting')}
            </Text>
          </Stack>
        ) : null}
        {job.error !== undefined && job.error !== '' ? (
          <Text c="red" size="sm">
            {job.error}
          </Text>
        ) : null}
        <SimpleGrid cols={{ base: 2, sm: 3, md: 6 }} spacing="md">
          <Counter
            label={t('healing.counters.scanned')}
            value={formatNumber(job.scanned, locale)}
          />
          <Counter label={t('healing.counters.healed')} value={formatNumber(job.healed, locale)} />
          <Counter label={t('healing.counters.failed')} value={formatNumber(job.failed, locale)} />
          <Counter
            label={t('healing.counters.dangling')}
            value={formatNumber(job.dangling, locale)}
          />
          <Counter
            label={t('healing.counters.removed')}
            value={formatNumber(job.removed, locale)}
          />
          <Counter label={t('healing.counters.bytes')} value={formatBytes(job.bytes, locale)} />
        </SimpleGrid>

        <Table.ScrollContainer minWidth={640}>
          <Table aria-label={t('healing.drives.title')} verticalSpacing="xs">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('healing.drives.drive')}</Table.Th>
                <Table.Th>{t('healing.drives.position')}</Table.Th>
                <Table.Th>{t('common.status')}</Table.Th>
                <Table.Th ta="right">{t('healing.drives.missing')}</Table.Th>
                <Table.Th ta="right">{t('healing.drives.corrupt')}</Table.Th>
                <Table.Th ta="right">{t('healing.drives.offline')}</Table.Th>
                <Table.Th ta="right">{t('healing.drives.healed')}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {job.drives.map((d) => (
                <Table.Tr
                  key={d.endpoint}
                  data-state={d.state}
                  style={
                    d.state === 'offline'
                      ? { backgroundColor: 'var(--mantine-color-red-light)' }
                      : undefined
                  }
                >
                  <Table.Td>
                    <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                      {d.endpoint}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">
                      {t('healing.drives.positionValue', {
                        pool: d.pool + 1,
                        set: d.set + 1,
                        disk: d.disk + 1,
                      })}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge color={d.state === 'online' ? 'green' : 'red'} variant="dot">
                      {t(`overview.states.${d.state}`)}
                    </Badge>
                  </Table.Td>
                  <Table.Td ta="right">{formatNumber(d.missing, locale)}</Table.Td>
                  <Table.Td ta="right">{formatNumber(d.corrupt, locale)}</Table.Td>
                  <Table.Td ta="right">{formatNumber(d.offline, locale)}</Table.Td>
                  <Table.Td ta="right">{formatNumber(d.healed, locale)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>

        {job.items.length > 0 ? (
          <Table.ScrollContainer minWidth={640}>
            <Table aria-label={t('healing.items.title')} verticalSpacing="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('healing.items.item')}</Table.Th>
                  <Table.Th>{t('healing.items.drives')}</Table.Th>
                  <Table.Th>{t('healing.items.result')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {job.items.map((item, i) => (
                  <Table.Tr
                    key={`${item.kind}:${item.bucket ?? ''}:${item.object ?? ''}:${String(i)}`}
                  >
                    <Table.Td>
                      <Text size="xs" c="dimmed">
                        {t(`healing.items.kinds.${item.kind}`)}
                      </Text>
                      <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                        {[item.bucket, item.object]
                          .filter((s) => s !== undefined && s !== '')
                          .join('/') || '—'}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="wrap">
                        {item.drives
                          .filter((d) => d.before !== 'ok' || d.after !== 'ok')
                          .map((d) => (
                            <Badge
                              key={d.endpoint}
                              color={driveStateColor[d.after]}
                              variant="outline"
                              tt="none"
                            >
                              {t('healing.items.driveChange', {
                                disk: d.disk + 1,
                                before: t(`healing.driveStates.${d.before}`),
                                after: t(`healing.driveStates.${d.after}`),
                              })}
                            </Badge>
                          ))}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <ItemResult item={item} />
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        ) : null}
        {job.itemsTruncated === true ? (
          <Text size="xs" c="dimmed">
            {t('healing.items.truncated')}
          </Text>
        ) : null}
      </Stack>
    </Paper>
  );
}
