import {
  Alert,
  Badge,
  Button,
  Group,
  Paper,
  Progress,
  Stack,
  Table,
  Text,
  Title,
} from '@mantine/core';
import { IconInfoCircle, IconServerBolt } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { PoolActions, usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { clusterRefreshMs, healRunningRefreshMs, poolsQueryKey } from '@/lib/cluster';
import { formatBytes, formatNumber } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { isApiError, useServices, type PoolJob, type Pools } from '@/services';

import { ExpandClusterWizard } from './ExpandClusterWizard';

/** Badge colours of the job states. */
const jobColor: Record<PoolJob['state'], string> = {
  running: 'blue',
  complete: 'green',
  canceled: 'gray',
  failed: 'red',
};

function running(pools: Pools | undefined): boolean {
  return (
    pools !== undefined &&
    (pools.rebalance?.state === 'running' ||
      pools.pools.some((p) => p.decommission?.state === 'running'))
  );
}

/** The progress of a decommission or of the rebalance. */
function JobProgress({ job }: { job: PoolJob }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const details = [
    t('clusterPools.moved', {
      count: job.objects,
      objects: formatNumber(job.objects, locale),
      bytes: formatBytes(job.bytes, locale),
    }),
  ];
  if (job.pass > 1) details.push(t('clusterPools.pass', { pass: job.pass }));
  if (job.failed > 0) details.push(t('clusterPools.failed', { count: job.failed }));
  if (job.pendingUploads > 0) {
    details.push(t('clusterPools.uploads', { count: job.pendingUploads }));
  }
  if (job.state === 'running' && job.bucket !== undefined && job.bucket !== '') {
    details.push(
      t('clusterPools.at', { position: `${job.bucket}/${job.object ?? ''}`.replace(/\/$/, '') }),
    );
  }
  return (
    <Stack gap={2}>
      <Group gap="xs">
        <Badge color={jobColor[job.state]} variant="light">
          {t(`clusterPools.states.${job.state}`)}
        </Badge>
      </Group>
      <Text size="xs" c="dimmed">
        {details.join(' · ')}
      </Text>
      {job.error !== undefined && job.error !== '' && job.state !== 'complete' ? (
        <Text size="xs" c="red" style={{ overflowWrap: 'anywhere' }}>
          {job.error}
        </Text>
      ) : null}
    </Stack>
  );
}

/**
 * D-CLU-03: the pools with their usage and their decommission, and the
 * rebalance between them. A decommission drains a pool (no new objects,
 * every version moved and verified elsewhere) so that it can be removed
 * from the command line; the first pool holds the system documents and
 * cannot be drained. Hidden in single-drive mode (no pools to manage).
 */
export function PoolManagement() {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { pools: service } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const [confirm, setConfirm] = useState<number | null>(null);
  const [expand, setExpand] = useState(false);

  const query = useQuery({
    queryKey: poolsQueryKey,
    queryFn: () => service.list(),
    refetchInterval: (q) => (running(q.state.data) ? healRunningRefreshMs : clusterRefreshMs),
    retry: (count, error) => !(isApiError(error) && error.status === 501) && count < 2,
  });
  const done = async () => {
    await queryClient.invalidateQueries({ queryKey: poolsQueryKey });
  };
  const decommission = useMutation({
    mutationFn: (pool: number) => service.decommission(pool),
    onSuccess: async () => {
      setConfirm(null);
      await done();
    },
  });
  const cancel = useMutation({
    mutationFn: (pool: number) => service.cancelDecommission(pool),
    onSuccess: done,
  });
  const rebalance = useMutation({
    mutationFn: (start: boolean) => (start ? service.startRebalance() : service.stopRebalance()),
    onSuccess: done,
  });

  if (query.isError && isApiError(query.error) && query.error.status === 501) {
    return null;
  }
  const title = t('clusterPools.title');
  const data = query.data;
  const canDecommission = perms.can(PoolActions.decommission);
  const canRebalance = perms.can(PoolActions.rebalance);
  const rebalanceJob = data?.rebalance;
  const rebalancing = rebalanceJob?.state === 'running';
  const actionError = cancel.error ?? rebalance.error;

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="md">
        <Group justify="space-between" align="flex-start" wrap="wrap">
          <Stack gap={2} maw={640}>
            <Title order={4}>{title}</Title>
            <Text size="sm" c="dimmed">
              {t('clusterPools.subtitle')}
            </Text>
          </Stack>
          {data ? (
            <Button
              size="xs"
              leftSection={<IconServerBolt size={14} />}
              onClick={() => {
                setExpand(true);
              }}
            >
              {t('clusterExpand.open')}
            </Button>
          ) : null}
        </Group>
        {query.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, query.error)}
          </Alert>
        ) : null}
        {actionError !== null ? <ErrorAlert error={actionError} /> : null}
        {data?.pools.some((p) => p.decommission?.state === 'complete') ? (
          <Alert color="green" variant="light" icon={<IconInfoCircle size={18} />}>
            <Group justify="space-between" gap="xs">
              <Text size="sm">{t('clusterPools.removeHint')}</Text>
              <Button
                size="xs"
                variant="light"
                color="green"
                onClick={() => {
                  setExpand(true);
                }}
              >
                {t('clusterExpand.planRemoval')}
              </Button>
            </Group>
          </Alert>
        ) : null}

        {data ? (
          <Table.ScrollContainer minWidth={720}>
            <Table aria-label={title} verticalSpacing="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('clusterPools.pool')}</Table.Th>
                  <Table.Th>{t('common.status')}</Table.Th>
                  <Table.Th>{t('cluster.drives.usage')}</Table.Th>
                  <Table.Th>{t('clusterPools.decommission')}</Table.Th>
                  <Table.Th>{t('common.actions')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.pools.map((p) => {
                  const used = p.total - p.free;
                  const ratio = p.total > 0 ? used / p.total : 0;
                  const job = p.decommission;
                  return (
                    <Table.Tr key={p.index} data-state={p.draining ? 'draining' : 'active'}>
                      <Table.Td>
                        <Text fw={600} size="sm">
                          {t('cluster.pool.title', { index: p.index })}
                        </Text>
                        <Text
                          size="xs"
                          c="dimmed"
                          ff="monospace"
                          style={{ overflowWrap: 'anywhere' }}
                        >
                          {p.id}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Badge color={p.draining ? 'orange' : 'green'} variant="light">
                          {t(p.draining ? 'clusterPools.draining' : 'clusterPools.active')}
                        </Badge>
                      </Table.Td>
                      <Table.Td miw={160}>
                        {p.total > 0 ? (
                          <>
                            <Progress
                              value={ratio * 100}
                              size="sm"
                              aria-label={t('clusterPools.usage', { index: p.index })}
                            />
                            <Text size="xs" c="dimmed" mt={2}>
                              {t('overview.nodes.usedOf', {
                                used: formatBytes(used, locale),
                                total: formatBytes(p.total, locale),
                              })}
                            </Text>
                          </>
                        ) : (
                          '—'
                        )}
                      </Table.Td>
                      <Table.Td>
                        {job ? (
                          <JobProgress job={job} />
                        ) : (
                          <Text size="sm" c="dimmed">
                            —
                          </Text>
                        )}
                      </Table.Td>
                      <Table.Td>
                        {job?.state === 'running' ? (
                          <Button
                            size="xs"
                            variant="default"
                            disabled={!canDecommission}
                            loading={cancel.isPending && cancel.variables === p.index}
                            onClick={() => {
                              cancel.mutate(p.index);
                            }}
                          >
                            {t('clusterPools.cancel', { index: p.index })}
                          </Button>
                        ) : p.index > 1 && job?.state !== 'complete' ? (
                          <Button
                            size="xs"
                            color="red"
                            variant="light"
                            disabled={!canDecommission}
                            onClick={() => {
                              decommission.reset();
                              setConfirm(p.index);
                            }}
                          >
                            {t('clusterPools.start', { index: p.index })}
                          </Button>
                        ) : null}
                      </Table.Td>
                    </Table.Tr>
                  );
                })}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        ) : null}

        {data && data.pools.length > 1 ? (
          <Group justify="space-between" align="flex-start" wrap="wrap">
            <Stack gap={4}>
              <Text fw={600} size="sm">
                {t('clusterPools.rebalance')}
              </Text>
              {rebalanceJob ? (
                <JobProgress job={rebalanceJob} />
              ) : (
                <Text size="sm" c="dimmed">
                  {t('clusterPools.rebalanceHint')}
                </Text>
              )}
            </Stack>
            <Button
              size="xs"
              variant="default"
              disabled={!canRebalance}
              loading={rebalance.isPending}
              onClick={() => {
                rebalance.mutate(!rebalancing);
              }}
            >
              {t(rebalancing ? 'clusterPools.stopRebalance' : 'clusterPools.startRebalance')}
            </Button>
          </Group>
        ) : null}
      </Stack>

      {data ? (
        <ExpandClusterWizard
          opened={expand}
          pools={data}
          onClose={() => {
            setExpand(false);
          }}
        />
      ) : null}
      <ConfirmDialog
        opened={confirm !== null}
        title={t('clusterPools.confirmTitle', { index: confirm ?? 0 })}
        message={t('clusterPools.confirm', { index: confirm ?? 0 })}
        confirmLabel={t('clusterPools.confirmLabel')}
        loading={decommission.isPending}
        error={decommission.isError ? decommission.error : undefined}
        onCancel={() => {
          setConfirm(null);
        }}
        onConfirm={() => {
          if (confirm !== null) decommission.mutate(confirm);
        }}
      />
    </Paper>
  );
}
