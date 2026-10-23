import {
  Alert,
  Badge,
  Button,
  Group,
  Modal,
  Paper,
  SegmentedControl,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { MaintenanceActions, usePermissions } from '@/auth/usePermissions';
import { ErrorAlert } from '@/components/iam/common';
import { clusterRefreshMs } from '@/lib/cluster';
import { useServices, type RestartMode, type UpdateJob } from '@/services';

const maintenanceJobKey = ['cluster', 'maintenance', 'job'] as const;

/** Poll interval while a job runs: the nodes restart one after the other. */
const runningRefreshMs = 2_000;

const stateColor: Record<UpdateJob['state'], string> = {
  running: 'blue',
  completed: 'green',
  failed: 'red',
};

const phaseColor: Record<UpdateJob['nodes'][number]['phase'], string> = {
  pending: 'gray',
  staged: 'cyan',
  restarting: 'blue',
  done: 'green',
  failed: 'red',
};

/** A timestamp of the job in the locale of the console. */
const formatDateTime = (iso: string, locale?: string) => new Date(iso).toLocaleString(locale);

const sha256Pattern = /^[0-9a-fA-F]{64}$/;
const urlPattern = /^https?:\/\/\S+$/;

/** The mode of a job: one node at a time or every node together. */
function ModeControl({
  value,
  onChange,
}: {
  value: RestartMode;
  onChange: (mode: RestartMode) => void;
}) {
  const { t } = useTranslation();
  return (
    <Stack gap={4}>
      <SegmentedControl
        value={value}
        onChange={(v) => {
          onChange(v as RestartMode);
        }}
        data={[
          { value: 'rolling', label: t('clusterMaintenance.rolling') },
          { value: 'all', label: t('clusterMaintenance.all') },
        ]}
        aria-label={t('clusterMaintenance.mode')}
      />
      <Text size="xs" c="dimmed">
        {t(value === 'rolling' ? 'clusterMaintenance.rollingHint' : 'clusterMaintenance.allHint')}
      </Text>
    </Stack>
  );
}

/** The last update or coordinated restart and the phase of every node. */
function JobDetails({ job }: { job: UpdateJob }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const title = t(`clusterMaintenance.kinds.${job.kind}`);
  return (
    <Stack gap="xs">
      <Group gap="xs">
        <Text fw={600} size="sm">
          {title}
        </Text>
        <Badge color={stateColor[job.state]} variant="light">
          {t(`clusterMaintenance.states.${job.state}`)}
        </Badge>
        <Badge color="gray" variant="outline">
          {t(job.mode === 'all' ? 'clusterMaintenance.all' : 'clusterMaintenance.rolling')}
        </Badge>
        {job.version ? (
          <Text size="sm" c="dimmed">
            {t('clusterMaintenance.toVersion', { version: job.version })}
          </Text>
        ) : null}
      </Group>
      <Text size="xs" c="dimmed">
        {t('clusterMaintenance.started', {
          time: formatDateTime(job.started, locale),
          node: job.node,
        })}
        {job.finished
          ? ` · ${t('clusterMaintenance.finished', { time: formatDateTime(job.finished, locale) })}`
          : ''}
      </Text>
      {job.error ? (
        <Alert color="red" variant="light" role="alert">
          {job.error}
        </Alert>
      ) : null}
      <Table.ScrollContainer minWidth={480}>
        <Table aria-label={t('clusterMaintenance.nodes')} verticalSpacing={4}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('clusterMaintenance.node')}</Table.Th>
              <Table.Th>{t('clusterMaintenance.phase')}</Table.Th>
              <Table.Th>{t('clusterMaintenance.version')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {job.nodes.map((n) => (
              <Table.Tr key={n.node}>
                <Table.Td>
                  <Text size="sm" ff="monospace">
                    {n.node}
                  </Text>
                  {n.error ? (
                    <Text size="xs" c="red" style={{ overflowWrap: 'anywhere' }}>
                      {n.error}
                    </Text>
                  ) : null}
                </Table.Td>
                <Table.Td>
                  <Badge color={phaseColor[n.phase]} variant="light">
                    {t(`clusterMaintenance.phases.${n.phase}`)}
                  </Badge>
                </Table.Td>
                <Table.Td>{n.version ?? '—'}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Stack>
  );
}

type Dialog = 'update' | 'restart' | 'stop' | null;

/**
 * Maintenance of the deployment (S-OPS-09, C-ADM-09): the update of the
 * binary of every node, the coordinated restart (rolling while the
 * cluster keeps its write quorum with one node down, or all together) and
 * the coordinated stop. The node that answers runs the job and restarts
 * last; the page follows the job across the restarts.
 */
export function ClusterMaintenance() {
  const { t } = useTranslation();
  const { maintenance } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const [dialog, setDialog] = useState<Dialog>(null);
  const [url, setUrl] = useState('');
  const [sha256, setSha256] = useState('');
  const [mode, setMode] = useState<RestartMode>('rolling');
  const [confirmText, setConfirmText] = useState('');
  const [submitted, setSubmitted] = useState(false);
  const [stopped, setStopped] = useState(false);

  const query = useQuery({
    queryKey: maintenanceJobKey,
    queryFn: () => maintenance.job(),
    refetchInterval: (q) =>
      q.state.data?.state === 'running' || q.state.status === 'error'
        ? runningRefreshMs
        : clusterRefreshMs * 3,
    retry: false,
  });
  const close = () => {
    setDialog(null);
    setSubmitted(false);
    setConfirmText('');
  };
  const started = async () => {
    close();
    await queryClient.invalidateQueries({ queryKey: maintenanceJobKey });
  };
  const update = useMutation({
    mutationFn: () => maintenance.update({ url: url.trim(), sha256: sha256.trim(), mode }),
    onSuccess: started,
  });
  const restart = useMutation({
    mutationFn: () => maintenance.restart(mode),
    onSuccess: started,
  });
  const stop = useMutation({
    mutationFn: () => maintenance.stop(),
    onSuccess: () => {
      close();
      setStopped(true);
    },
  });

  const job = query.data ?? null;
  const running = job?.state === 'running';
  const title = t('clusterMaintenance.title');
  const open = (d: Dialog) => {
    update.reset();
    restart.reset();
    stop.reset();
    setMode('rolling');
    setDialog(d);
  };
  const urlError = urlPattern.test(url.trim()) ? undefined : t('clusterMaintenance.urlInvalid');
  const shaError = sha256Pattern.test(sha256.trim())
    ? undefined
    : t('clusterMaintenance.sha256Invalid');
  const stopWord = t('clusterMaintenance.stopWord');

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="md">
        <Group justify="space-between" align="flex-start" wrap="wrap">
          <Stack gap={2} maw={640}>
            <Title order={4}>{title}</Title>
            <Text size="sm" c="dimmed">
              {t('clusterMaintenance.subtitle')}
            </Text>
          </Stack>
          <Group gap="xs">
            <Button
              size="xs"
              disabled={running || !perms.can(MaintenanceActions.update)}
              onClick={() => {
                open('update');
              }}
            >
              {t('clusterMaintenance.update')}
            </Button>
            <Button
              size="xs"
              variant="default"
              disabled={running || !perms.can(MaintenanceActions.restart)}
              onClick={() => {
                open('restart');
              }}
            >
              {t('clusterMaintenance.restart')}
            </Button>
            <Button
              size="xs"
              color="red"
              variant="light"
              disabled={running || !perms.can(MaintenanceActions.stop)}
              onClick={() => {
                open('stop');
              }}
            >
              {t('clusterMaintenance.stop')}
            </Button>
          </Group>
        </Group>

        {stopped ? (
          <Alert color="orange" variant="light" icon={<IconAlertTriangle size={18} />}>
            {t('clusterMaintenance.stopped')}
          </Alert>
        ) : null}
        {query.isError && !stopped ? (
          <Text size="sm" c="dimmed" role="status">
            {t('clusterMaintenance.unreachable')}
          </Text>
        ) : null}
        {job ? (
          <JobDetails job={job} />
        ) : query.isSuccess ? (
          <Text size="sm" c="dimmed">
            {t('clusterMaintenance.none')}
          </Text>
        ) : null}
      </Stack>

      <Modal
        opened={dialog === 'update'}
        onClose={close}
        title={t('clusterMaintenance.updateTitle')}
        size="lg"
      >
        <form
          onSubmit={(e) => {
            e.preventDefault();
            setSubmitted(true);
            if (urlError === undefined && shaError === undefined) update.mutate();
          }}
        >
          <Stack gap="sm">
            <Text size="sm" c="dimmed">
              {t('clusterMaintenance.updateHint')}
            </Text>
            <TextInput
              label={t('clusterMaintenance.url')}
              placeholder="https://github.com/R3n3r0/Nexo/releases/download/v1.1.0/nexo_linux_amd64"
              withAsterisk
              value={url}
              error={submitted ? urlError : undefined}
              onChange={(e) => {
                setUrl(e.currentTarget.value);
              }}
            />
            <TextInput
              label={t('clusterMaintenance.sha256')}
              description={t('clusterMaintenance.sha256Hint')}
              withAsterisk
              ff="monospace"
              value={sha256}
              error={submitted ? shaError : undefined}
              onChange={(e) => {
                setSha256(e.currentTarget.value);
              }}
            />
            <ModeControl value={mode} onChange={setMode} />
            {update.isError ? <ErrorAlert error={update.error} /> : null}
            <Group justify="flex-end" gap="xs">
              <Button variant="default" onClick={close}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" loading={update.isPending}>
                {t('clusterMaintenance.updateSubmit')}
              </Button>
            </Group>
          </Stack>
        </form>
      </Modal>

      <Modal
        opened={dialog === 'restart'}
        onClose={close}
        title={t('clusterMaintenance.restartTitle')}
      >
        <Stack gap="sm">
          <Text size="sm">{t('clusterMaintenance.restartHint')}</Text>
          <ModeControl value={mode} onChange={setMode} />
          {restart.isError ? <ErrorAlert error={restart.error} /> : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={close}>
              {t('common.cancel')}
            </Button>
            <Button
              loading={restart.isPending}
              onClick={() => {
                restart.mutate();
              }}
            >
              {t('clusterMaintenance.restartSubmit')}
            </Button>
          </Group>
        </Stack>
      </Modal>

      <Modal opened={dialog === 'stop'} onClose={close} title={t('clusterMaintenance.stopTitle')}>
        <Stack gap="sm">
          <Alert color="red" variant="light" icon={<IconAlertTriangle size={18} />}>
            {t('clusterMaintenance.stopHint')}
          </Alert>
          <TextInput
            label={t('clusterMaintenance.stopConfirm', { word: stopWord })}
            value={confirmText}
            onChange={(e) => {
              setConfirmText(e.currentTarget.value);
            }}
          />
          {stop.isError ? <ErrorAlert error={stop.error} /> : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={close}>
              {t('common.cancel')}
            </Button>
            <Button
              color="red"
              disabled={confirmText.trim().toLowerCase() !== stopWord.toLowerCase()}
              loading={stop.isPending}
              onClick={() => {
                stop.mutate();
              }}
            >
              {t('clusterMaintenance.stopSubmit')}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Paper>
  );
}
