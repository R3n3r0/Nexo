import {
  Alert,
  Badge,
  Button,
  Group,
  Paper,
  Skeleton,
  Stack,
  Table,
  Text,
  Title,
  VisuallyHidden,
} from '@mantine/core';
import { IconInfoCircle } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import {
  clusterRefreshMs,
  healIdleRefreshMs,
  healKeys,
  healOptions,
  healRunningRefreshMs,
  healTarget,
  isRunning,
  jobStateColor,
} from '@/lib/cluster';
import { formatNumber } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { serverInfoQueryKey } from '@/lib/serverInfo';
import { isApiError, useServices, type HealJob } from '@/services';

import { BackgroundHealing } from './BackgroundHealing';
import { HealJobDetails } from './HealJobDetails';
import { StartHealForm } from './StartHealForm';

function JobsTable({
  jobs,
  selected,
  onSelect,
}: {
  jobs: HealJob[];
  selected: string | undefined;
  onSelect: (id: string) => void;
}) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const title = t('healing.jobs.title');
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Title order={4} mb="sm">
        {title}
      </Title>
      {jobs.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t('healing.jobs.empty')}
        </Text>
      ) : (
        <Table.ScrollContainer minWidth={760}>
          <Table aria-label={title} verticalSpacing="xs" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('healing.jobs.started')}</Table.Th>
                <Table.Th>{t('healing.jobs.target')}</Table.Th>
                <Table.Th>{t('healing.jobs.options')}</Table.Th>
                <Table.Th>{t('common.status')}</Table.Th>
                <Table.Th ta="right">{t('healing.counters.scanned')}</Table.Th>
                <Table.Th ta="right">{t('healing.counters.healed')}</Table.Th>
                <Table.Th ta="right">{t('healing.counters.failed')}</Table.Th>
                <Table.Th>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {jobs.map((job) => {
                const target = healTarget(job.request);
                return (
                  <Table.Tr key={job.id} data-selected={job.id === selected ? 'true' : undefined}>
                    <Table.Td>
                      <Text size="sm">{new Date(job.startedAt).toLocaleString(locale)}</Text>
                      <Text size="xs" c="dimmed" ff="monospace">
                        {job.id}
                      </Text>
                    </Table.Td>
                    <Table.Td style={{ overflowWrap: 'anywhere' }}>
                      {target === undefined ? t('healing.target.all') : target}
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>
                        {healOptions(job.request).map((o) => (
                          <Badge key={o} variant="outline" size="sm">
                            {t(`healing.badges.${o}`)}
                          </Badge>
                        ))}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Badge color={jobStateColor[job.state]} variant="light">
                        {t(`healing.states.${job.state}`)}
                      </Badge>
                    </Table.Td>
                    <Table.Td ta="right">{formatNumber(job.scanned, locale)}</Table.Td>
                    <Table.Td ta="right">{formatNumber(job.healed, locale)}</Table.Td>
                    <Table.Td ta="right">{formatNumber(job.failed, locale)}</Table.Td>
                    <Table.Td>
                      <Button
                        size="xs"
                        variant={job.id === selected ? 'light' : 'subtle'}
                        aria-label={t('healing.jobs.showDetails', { id: job.id })}
                        onClick={() => {
                          onSelect(job.id);
                        }}
                      >
                        {t('healing.jobs.details')}
                      </Button>
                    </Table.Td>
                  </Table.Tr>
                );
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Paper>
  );
}

/**
 * D-CLU-02: starts healing jobs with their options, follows the running
 * and recent ones (progress, objects repaired, results drive by drive) and
 * shows the healing the cluster does by itself (background queue, drives
 * being repopulated). A single drive has nothing to heal.
 */
export function HealingPage() {
  const { t } = useTranslation();
  const { heal, info } = useServices();
  const [selected, setSelected] = useState<string | undefined>(undefined);
  const [started, setStarted] = useState<HealJob | undefined>(undefined);
  const jobs = useQuery({
    queryKey: healKeys.jobs,
    queryFn: () => heal.list(),
    refetchInterval: (q) =>
      q.state.data?.some(isRunning) === true ? healRunningRefreshMs : healIdleRefreshMs,
  });
  const server = useQuery({
    queryKey: serverInfoQueryKey,
    queryFn: () => info.serverInfo(),
    refetchInterval: clusterRefreshMs,
  });
  const unsupported = jobs.isError && isApiError(jobs.error) && jobs.error.status === 501;
  const current = selected ?? jobs.data?.[0]?.id;

  return (
    <Stack gap="lg">
      <Stack gap={2}>
        <Title order={2}>{t('healing.title')}</Title>
        <Text c="dimmed" size="sm">
          {t('healing.subtitle')}
        </Text>
      </Stack>

      {unsupported ? (
        <Paper withBorder radius="md" p="md" role="status">
          <Group gap="sm" wrap="nowrap" align="flex-start">
            <IconInfoCircle size={18} aria-hidden />
            <Text>{t('healing.unsupported')}</Text>
          </Group>
        </Paper>
      ) : jobs.isError ? (
        <Alert color="red" variant="light" role="alert">
          {problemMessage(t, jobs.error)}
        </Alert>
      ) : null}

      <StartHealForm
        disabled={unsupported}
        onStarted={(job) => {
          setStarted(job);
          setSelected(job.id);
        }}
      />

      {current !== undefined ? (
        <HealJobDetails
          key={current}
          id={current}
          {...(started?.id === current ? { initial: started } : {})}
        />
      ) : null}

      {jobs.isPending ? (
        <Skeleton height={120} />
      ) : jobs.data ? (
        <JobsTable jobs={jobs.data} selected={current} onSelect={setSelected} />
      ) : null}

      {server.data && !unsupported ? <BackgroundHealing info={server.data} /> : null}
    </Stack>
  );
}
