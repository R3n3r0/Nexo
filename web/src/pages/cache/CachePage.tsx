import {
  Alert,
  Anchor,
  Badge,
  Button,
  Group,
  Paper,
  Select,
  Skeleton,
  Stack,
  Switch,
  Table,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import {
  IconAlertTriangle,
  IconCopy,
  IconDeviceFloppy,
  IconPlus,
  IconRefresh,
  IconTrash,
} from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, Navigate, useParams } from 'react-router';

import { usePermissions } from '@/auth/usePermissions';
import { durationText } from '@/components/cache/helpers';
import { InspectorPanel } from '@/components/cache/InspectorPanel';
import {
  InvalidationForm,
  InvalidationHistory,
  InvalidationProgress,
} from '@/components/cache/Invalidations';
import { SettingsFields } from '@/components/cache/SettingsFields';
import { StatsView } from '@/components/cache/StatsView';
import { WarmPanel } from '@/components/cache/WarmPanel';
import { useDistributionChoices } from '@/components/cache/useDistributionChoices';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { cacheKeys, pruneSettings, specOf, validateSpec } from '@/lib/cache';
import { problemMessage } from '@/lib/problem';
import {
  isApiError,
  useServices,
  type CacheGlobalSettings,
  type CacheInvalidation,
} from '@/services';

import { CacheHeader } from './CacheHeader';
import { cacheTabs, visibleCacheTabs, type CacheTab } from './cacheTabs';
import { ScrollTable } from '@/components/cache/ScrollTable';

/** D-CCH-01 and D-CCH-08: the statistics of the cluster, or of one distribution. */
function OverviewTab() {
  const { t } = useTranslation();
  const { cache } = useServices();
  const [distribution, setDistribution] = useState('');
  const all = useQuery({
    queryKey: cacheKeys.stats('', ''),
    queryFn: () => cache.stats({ top: 10 }),
    refetchInterval: 15_000,
  });
  const filtered = useQuery({
    queryKey: cacheKeys.stats(distribution, ''),
    queryFn: () => cache.stats({ distribution, top: 10 }),
    enabled: distribution !== '',
    refetchInterval: 15_000,
  });
  const query = distribution === '' ? all : filtered;
  const distributions = [...new Set((all.data?.hosts ?? []).map((h) => h.distribution))].sort();
  return (
    <Stack gap="md">
      <Group justify="space-between" align="flex-end" wrap="wrap">
        <Select
          label={t('cache.overview.filter')}
          data={[
            { value: '', label: t('cache.overview.allDistributions') },
            ...distributions.map((d) => ({ value: d, label: d })),
          ]}
          value={distribution}
          allowDeselect={false}
          onChange={(v) => {
            setDistribution(v ?? '');
          }}
          style={{ minWidth: 240 }}
        />
        <Button
          variant="default"
          leftSection={<IconRefresh size={16} />}
          loading={query.isFetching}
          onClick={() => void query.refetch()}
        >
          {t('cache.overview.refresh')}
        </Button>
      </Group>
      {query.isPending ? (
        <Skeleton height={320} />
      ) : query.isError ? (
        <ErrorAlert
          error={query.error}
          title={t('cache.stats.loadError')}
          onRetry={() => void query.refetch()}
        />
      ) : (
        <StatsView stats={query.data} global={distribution === ''} />
      )}
    </Stack>
  );
}

/** D-CCH-03: the built-in and the defined policies. */
function PoliciesTab() {
  const { t } = useTranslation();
  const { cache } = useServices();
  const queryClient = useQueryClient();
  const [removing, setRemoving] = useState<string | null>(null);
  const list = useQuery({ queryKey: cacheKeys.policies, queryFn: () => cache.listPolicies() });
  const remove = useMutation({
    mutationFn: (name: string) => cache.deletePolicy(name),
    onSuccess: async (_, name) => {
      setRemoving(null);
      notifications.show({ color: 'green', message: t('cache.policies.deleted', { name }) });
      await queryClient.invalidateQueries({ queryKey: cacheKeys.policies });
    },
  });
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('cache.policies.title')}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Title order={4}>{t('cache.policies.title')}</Title>
          <Button component={Link} to="/cache/policies/-/new" leftSection={<IconPlus size={16} />}>
            {t('cache.policies.create')}
          </Button>
        </Group>
        <Text size="sm" c="dimmed">
          {t('cache.policies.hint')}
        </Text>
        {list.isPending ? (
          <Skeleton height={160} />
        ) : list.isError ? (
          <ErrorAlert error={list.error} title={t('cache.policies.loadError')} />
        ) : (
          <ScrollTable minWidth={760} label={t('cache.policies.title')}>
            <Table aria-label={t('cache.policies.title')} verticalSpacing={6}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('common.name')}</Table.Th>
                  <Table.Th>{t('cache.policies.type')}</Table.Th>
                  <Table.Th>{t('cache.policies.ttl')}</Table.Th>
                  <Table.Th>{t('cache.policies.cacheControl')}</Table.Th>
                  <Table.Th>{t('cache.policies.comment')}</Table.Th>
                  <Table.Th>{t('common.actions')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {list.data.map((p) => (
                  <Table.Tr key={p.name}>
                    <Table.Td>
                      <Anchor
                        component={Link}
                        to={`/cache/policies/${encodeURIComponent(p.name)}`}
                        ff="monospace"
                        size="sm"
                      >
                        {p.name}
                      </Anchor>
                    </Table.Td>
                    <Table.Td>
                      <Badge variant="light" color={p.builtin === true ? 'gray' : 'blue'}>
                        {p.builtin === true
                          ? t('cache.policies.builtin')
                          : t('cache.policies.defined')}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">
                        {p.bypass?.always === true
                          ? t('cache.policies.noCaching')
                          : t('cache.policies.ttlValue', {
                              default: durationText(p.ttl?.default),
                              max: durationText(p.ttl?.max),
                            })}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" ff="monospace">
                        {(p.browserCacheControl ?? '') === ''
                          ? t('cache.policies.objectCacheControl')
                          : p.browserCacheControl}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed">
                        {p.comment ?? ''}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="nowrap">
                        <Button
                          component={Link}
                          to={`/cache/policies/-/new?from=${encodeURIComponent(p.name)}`}
                          size="compact-xs"
                          variant="subtle"
                          leftSection={<IconCopy size={12} />}
                          aria-label={t('cache.policies.duplicateOf', { name: p.name })}
                        >
                          {t('cache.policies.duplicate')}
                        </Button>
                        {p.builtin !== true ? (
                          <Button
                            size="compact-xs"
                            variant="subtle"
                            color="red"
                            leftSection={<IconTrash size={12} />}
                            aria-label={t('cache.policies.deleteOf', { name: p.name })}
                            onClick={() => {
                              remove.reset();
                              setRemoving(p.name);
                            }}
                          >
                            {t('common.delete')}
                          </Button>
                        ) : null}
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </ScrollTable>
        )}
      </Stack>
      <ConfirmDialog
        opened={removing !== null}
        title={t('cache.policies.deleteTitle')}
        message={t('cache.policies.deleteConfirm', { name: removing ?? '' })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setRemoving(null);
        }}
        onConfirm={() => {
          if (removing !== null) remove.mutate(removing);
        }}
      />
    </Paper>
  );
}

/** D-CCH-04: invalidations of a distribution, the global purge and their history. */
function InvalidationsTab() {
  const { t } = useTranslation();
  const { cache } = useServices();
  const queryClient = useQueryClient();
  const choices = useDistributionChoices();
  const [selected, setSelected] = useState('');
  const [purging, setPurging] = useState(false);
  const [purged, setPurged] = useState<CacheInvalidation | null>(null);
  const purge = useMutation({
    mutationFn: () => cache.invalidate('', { all: true }),
    onSuccess: async (inv) => {
      setPurging(false);
      setPurged(inv);
      await queryClient.invalidateQueries({ queryKey: cacheKeys.invalidations('') });
    },
  });
  return (
    <Stack gap="md">
      <InvalidationForm choices={choices} onDistributionChange={setSelected} />
      {selected !== '' ? <InvalidationHistory key={selected} distribution={selected} /> : null}
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('cache.invalidations.globalTitle')}
      >
        <Stack gap="sm">
          <Group justify="space-between" wrap="wrap">
            <Title order={4}>{t('cache.invalidations.globalTitle')}</Title>
            <Button
              color="red"
              variant="light"
              leftSection={<IconTrash size={16} />}
              onClick={() => {
                purge.reset();
                setPurging(true);
              }}
            >
              {t('cache.invalidations.purgeEverything')}
            </Button>
          </Group>
          <Text size="sm" c="dimmed">
            {t('cache.invalidations.globalHint')}
          </Text>
        </Stack>
      </Paper>
      {purged !== null ? (
        <InvalidationProgress key={purged.id} distribution="" invalidation={purged} />
      ) : null}
      <InvalidationHistory distribution="" />
      <ConfirmDialog
        opened={purging}
        title={t('cache.invalidations.purgeEverythingTitle')}
        message={t('cache.invalidations.purgeEverythingConfirm')}
        confirmLabel={t('cache.invalidations.purgeEverything')}
        loading={purge.isPending}
        error={purge.isError ? purge.error : undefined}
        onCancel={() => {
          setPurging(false);
        }}
        onConfirm={() => {
          purge.mutate();
        }}
      />
    </Stack>
  );
}

/** The settings of the cluster (global level). */
function SettingsTab() {
  const { t } = useTranslation();
  const { cache } = useServices();
  const queryClient = useQueryClient();
  const settings = useQuery({ queryKey: cacheKeys.settings, queryFn: () => cache.getSettings() });
  const policies = useQuery({ queryKey: cacheKeys.policies, queryFn: () => cache.listPolicies() });
  const [draft, setDraft] = useState<CacheGlobalSettings | null>(null);
  const base = settings.data;
  const current = draft ?? base;
  const save = useMutation({
    mutationFn: (s: CacheGlobalSettings) =>
      cache.updateSettings({ ...s, cache: pruneSettings(s.cache) }, base?.etag),
    onSuccess: (stored) => {
      queryClient.setQueryData(cacheKeys.settings, stored);
      setDraft(null);
      notifications.show({ color: 'green', message: t('cache.globalSettings.saved') });
    },
  });
  const conflict = isApiError(save.error) && save.error.isPreconditionFailed;
  if (settings.isError) {
    return <ErrorAlert error={settings.error} title={t('cache.globalSettings.loadError')} />;
  }
  if (current === undefined) return <Skeleton height={320} />;
  const change = (next: CacheGlobalSettings) => {
    save.reset();
    setDraft(next);
  };
  const debug = current.debug ?? {};
  const invalid = validateSpec(specOf(current.cache)).length > 0;
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('cache.globalSettings.title')}
    >
      <Stack gap="sm">
        <Title order={4}>{t('cache.globalSettings.title')}</Title>
        <Text size="sm" c="dimmed">
          {t('cache.globalSettings.hint')}
        </Text>
        <SettingsFields
          level="global"
          settings={current.cache}
          policies={(policies.data ?? []).map((p) => p.name)}
          inherited={{ enabled: true, policy: 'standard' }}
          onChange={(c) => {
            change({ ...current, cache: c });
          }}
        />
        <Switch
          label={t('cache.globalSettings.nodeHeader')}
          description={t('cache.globalSettings.nodeHeaderHint')}
          checked={current.nodeHeader === true}
          onChange={(event) => {
            change({ ...current, nodeHeader: event.currentTarget.checked });
          }}
        />
        <Group grow align="flex-start" wrap="wrap">
          <TextInput
            label={t('cache.globalSettings.debugHeader')}
            description={t('cache.globalSettings.debugHeaderHint')}
            placeholder="X-Nexo-Cache-Debug"
            value={debug.header ?? ''}
            onChange={(event) => {
              const header = event.currentTarget.value;
              change({
                ...current,
                debug: { ...debug, header: header === '' ? undefined : header },
              });
            }}
          />
          <TextInput
            label={t('cache.globalSettings.debugSecret')}
            description={t('cache.globalSettings.debugSecretHint')}
            type="password"
            autoComplete="new-password"
            value={debug.secret ?? ''}
            onChange={(event) => {
              const secret = event.currentTarget.value;
              change({
                ...current,
                debug: { ...debug, secret: secret === '' ? undefined : secret },
              });
            }}
          />
        </Group>
        {conflict ? (
          <Alert
            color="yellow"
            variant="light"
            icon={<IconAlertTriangle size={18} />}
            title={t('cache.tab.conflictTitle')}
          >
            {t('cache.globalSettings.conflict')}
          </Alert>
        ) : save.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, save.error)}
          </Alert>
        ) : null}
        <Group justify="flex-end" gap="xs">
          <Button
            variant="default"
            disabled={draft === null && !conflict}
            onClick={() => {
              setDraft(null);
              save.reset();
              if (conflict) void settings.refetch();
            }}
          >
            {conflict ? t('cache.tab.reload') : t('cache.tab.discard')}
          </Button>
          <Button
            leftSection={<IconDeviceFloppy size={16} />}
            loading={save.isPending}
            disabled={draft === null || invalid || conflict}
            onClick={() => {
              if (draft !== null) save.mutate(draft);
            }}
          >
            {t('cache.tab.save')}
          </Button>
        </Group>
      </Stack>
    </Paper>
  );
}

/**
 * The cache section (D-CCH-01..07): the statistics of the cluster, the
 * cache policies, the invalidations, the warm-ups, the URL inspector and
 * the settings of the cluster. A user with admin:InvalidateCache alone
 * sees the invalidations and the warm-ups only (TD-C-06).
 */
export function CachePage() {
  const { t } = useTranslation();
  const { tab } = useParams();
  const perms = usePermissions();
  const choices = useDistributionChoices();
  if (!perms.ready) {
    return perms.loading ? (
      <Skeleton height={320} />
    ) : (
      <Alert color="red" variant="light" role="alert">
        {t('cache.permissionsError')}
      </Alert>
    );
  }
  const tabs = visibleCacheTabs(perms);
  if (tabs.length === 0) {
    return (
      <Stack gap="md">
        <CacheHeader active="overview" tabs={[]} />
        <Alert color="gray" variant="light" role="alert">
          {t('cache.forbidden')}
        </Alert>
      </Stack>
    );
  }
  const first = tabs[0] as CacheTab;
  if (tab === undefined) return <Navigate to={`/cache/${first}`} replace />;
  const known = (cacheTabs as readonly string[]).includes(tab);
  const active = tab as CacheTab;
  if (!known || !tabs.includes(active)) {
    return (
      <Stack gap="md">
        <CacheHeader active={first} tabs={tabs} />
        <Alert color="gray" variant="light" role="alert">
          {known
            ? t('cache.tabForbidden', { section: t(`cache.tabs.${active}`) })
            : t('cache.unknownTab')}
        </Alert>
      </Stack>
    );
  }
  return (
    <Stack gap="md">
      <CacheHeader active={active} tabs={tabs} />
      {active === 'overview' ? (
        <OverviewTab />
      ) : active === 'policies' ? (
        <PoliciesTab />
      ) : active === 'invalidations' ? (
        <InvalidationsTab />
      ) : active === 'warm' ? (
        <WarmPanel choices={choices} />
      ) : active === 'inspector' ? (
        <InspectorPanel choices={choices} />
      ) : (
        <SettingsTab />
      )}
    </Stack>
  );
}
