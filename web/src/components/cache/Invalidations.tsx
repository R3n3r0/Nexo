import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Paper,
  Progress,
  Select,
  Skeleton,
  Stack,
  Switch,
  Table,
  TagsInput,
  Text,
  Textarea,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Fragment, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { PatternField } from '@/components/accessRules/PatternField';
import { ChoiceField } from '@/components/common/ChoiceField';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { cacheKeys } from '@/lib/cache';
import { patternIssue } from '@/lib/keyPattern';
import { problemMessage } from '@/lib/problem';
import {
  useServices,
  type CacheInvalidation,
  type CacheInvalidationRequest,
  type CacheNodeStatus,
  type Distribution,
  type KeyPattern,
} from '@/services';

import { DistributionField } from './DistributionField';
import { invalidationScope, pollMs } from './helpers';
import { ScrollTable } from './ScrollTable';

type Scope = 'paths' | 'patterns' | 'tags' | 'domain' | 'all';

const nodeColors: Record<CacheNodeStatus['state'], string> = {
  pending: 'gray',
  running: 'blue',
  completed: 'teal',
  failed: 'red',
};

/** The state of an operation on one node. */
export function NodeStateBadge({ state }: { state: CacheNodeStatus['state'] }) {
  const { t } = useTranslation();
  return (
    <Badge size="sm" variant="light" color={nodeColors[state]} data-state={state}>
      {t(`cache.nodeStates.${state}`)}
    </Badge>
  );
}

/** The state of an invalidation: in progress or completed, with the nodes done. */
export function InvalidationStateBadge({ invalidation }: { invalidation: CacheInvalidation }) {
  const { t } = useTranslation();
  const done = invalidation.nodes.filter((n) => n.state === 'completed').length;
  const completed = invalidation.state === 'completed';
  return (
    <Badge
      variant="light"
      color={completed ? 'teal' : 'blue'}
      data-state={invalidation.state}
      aria-label={t('cache.invalidations.stateLabel', {
        state: t(`cache.invalidations.states.${invalidation.state}`),
        done,
        total: invalidation.nodes.length,
      })}
    >
      {t(`cache.invalidations.states.${invalidation.state}`)} ·{' '}
      {t('cache.invalidations.nodesDone', { done, total: invalidation.nodes.length })}
    </Badge>
  );
}

/** The state of an invalidation on every node. */
export function NodeStatusTable({
  nodes,
  label,
}: {
  nodes: readonly CacheNodeStatus[];
  label: string;
}) {
  const { t, i18n } = useTranslation();
  return (
    <Table aria-label={label} verticalSpacing={4}>
      <Table.Thead>
        <Table.Tr>
          <Table.Th>{t('cache.nodes.node')}</Table.Th>
          <Table.Th>{t('cache.nodes.state')}</Table.Th>
          <Table.Th ta="right">{t('cache.nodes.removed')}</Table.Th>
          <Table.Th>{t('cache.nodes.at')}</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {nodes.map((n) => (
          <Table.Tr key={n.node}>
            <Table.Td>
              <Text size="sm" ff="monospace">
                {n.node}
              </Text>
            </Table.Td>
            <Table.Td>
              <Group gap={4}>
                <NodeStateBadge state={n.state} />
                {n.error !== undefined && n.error !== '' ? (
                  <Text size="xs" c="red">
                    {n.error}
                  </Text>
                ) : null}
              </Group>
            </Table.Td>
            <Table.Td ta="right">{n.removed ?? '—'}</Table.Td>
            <Table.Td>
              <Text size="xs" c="dimmed">
                {n.at !== undefined ? new Date(n.at).toLocaleString(i18n.resolvedLanguage) : '—'}
              </Text>
            </Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}

/**
 * An invalidation followed until every node completes it (TD-C-03): read
 * again every second while in progress.
 */
export function InvalidationProgress({
  distribution,
  invalidation,
}: {
  distribution: string;
  invalidation: CacheInvalidation;
}) {
  const { t } = useTranslation();
  const { cache } = useServices();
  const query = useQuery({
    queryKey: cacheKeys.invalidation(distribution, invalidation.id),
    queryFn: () => cache.invalidation(distribution, invalidation.id),
    initialData: invalidation,
    refetchInterval: (q) => (q.state.data?.state === 'completed' ? false : pollMs),
  });
  const inv = query.data;
  const done = inv.nodes.filter((n) => n.state === 'completed').length;
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('cache.invalidations.progressTitle', { id: inv.id })}
    >
      <Stack gap="xs">
        <Group justify="space-between" wrap="wrap">
          <Text fw={500} size="sm">
            {t('cache.invalidations.progressTitle', { id: inv.id })}
          </Text>
          <InvalidationStateBadge invalidation={inv} />
        </Group>
        <Text size="sm" c="dimmed">
          {invalidationScope(t, inv.request)}
        </Text>
        <Progress
          value={inv.nodes.length === 0 ? 100 : (done / inv.nodes.length) * 100}
          aria-label={t('cache.invalidations.nodesDone', { done, total: inv.nodes.length })}
          color={inv.state === 'completed' ? 'teal' : 'blue'}
        />
        <NodeStatusTable
          nodes={inv.nodes}
          label={t('cache.invalidations.nodesTable', { id: inv.id })}
        />
      </Stack>
    </Paper>
  );
}

export interface InvalidationFormProps {
  /** The distribution purged; undefined: chosen in the form. */
  distribution?: string;
  /** The distributions to choose from (when not fixed); undefined: typed. */
  choices?: readonly Distribution[] | undefined;
  /** The domains offered (aliases of the distribution); undefined: typed. */
  domains?: readonly string[] | undefined;
  /** The distribution chosen in the form changed. */
  onDistributionChange?: (id: string) => void;
}

/**
 * D-CCH-04: invalidates cached content of a distribution by exact paths,
 * patterns (prefix, glob, regex), tags, a whole domain or everything, as
 * a soft purge (entries marked expired) or a hard one; the new
 * invalidation is followed node by node.
 */
export function InvalidationForm({
  distribution,
  choices,
  domains,
  onDistributionChange,
}: InvalidationFormProps) {
  const { t } = useTranslation();
  const { cache } = useServices();
  const queryClient = useQueryClient();
  const [chosen, setChosen] = useState('');
  const dist = distribution ?? chosen;
  const domainOptions = domains ?? choices?.find((d) => d.id === dist)?.aliases;
  const [scope, setScope] = useState<Scope>('paths');
  const [paths, setPaths] = useState('');
  const [patterns, setPatterns] = useState<(KeyPattern | undefined)[]>([
    { type: 'glob', pattern: '' },
  ]);
  const [tags, setTags] = useState<string[]>([]);
  const [domain, setDomain] = useState('');
  const [soft, setSoft] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [created, setCreated] = useState<{ dist: string; inv: CacheInvalidation } | null>(null);

  const request: CacheInvalidationRequest = {
    ...(domain.trim() !== '' && scope !== 'all' ? { domain: domain.trim() } : {}),
    ...(scope === 'paths'
      ? {
          paths: paths
            .split('\n')
            .map((p) => p.trim())
            .filter((p) => p !== ''),
        }
      : {}),
    ...(scope === 'patterns'
      ? {
          patterns: patterns.filter(
            (p): p is KeyPattern => p !== undefined && p.pattern.trim() !== '',
          ),
        }
      : {}),
    ...(scope === 'tags' ? { tags } : {}),
    ...(scope === 'all' ? { all: true } : {}),
    ...(soft ? { soft: true } : {}),
  };
  const empty =
    (scope === 'paths' && (request.paths ?? []).length === 0) ||
    (scope === 'patterns' && (request.patterns ?? []).length === 0) ||
    (scope === 'tags' && tags.length === 0) ||
    (scope === 'domain' && domain.trim() === '');
  const invalidPattern =
    scope === 'patterns' && patterns.some((p) => p !== undefined && patternIssue(p) !== undefined);

  const submit = useMutation({
    mutationFn: () => cache.invalidate(dist, request),
    onSuccess: async (inv) => {
      setConfirming(false);
      setCreated({ dist, inv });
      await queryClient.invalidateQueries({ queryKey: cacheKeys.invalidations(dist) });
    },
  });

  const domainField =
    domainOptions === undefined ? (
      <TextInput
        label={
          scope === 'domain'
            ? t('cache.invalidations.domain')
            : t('cache.invalidations.limitDomain')
        }
        description={scope === 'domain' ? undefined : t('cache.invalidations.limitDomainHint')}
        value={domain}
        onChange={(event) => {
          setDomain(event.currentTarget.value);
        }}
      />
    ) : (
      <Select
        label={
          scope === 'domain'
            ? t('cache.invalidations.domain')
            : t('cache.invalidations.limitDomain')
        }
        description={scope === 'domain' ? undefined : t('cache.invalidations.limitDomainHint')}
        clearable={scope !== 'domain'}
        placeholder={scope === 'domain' ? undefined : t('cache.invalidations.allDomains')}
        data={[...domainOptions]}
        value={domain === '' ? null : domain}
        onChange={(v) => {
          setDomain(v ?? '');
        }}
      />
    );

  return (
    <Stack gap="md">
      <Paper
        withBorder
        radius="md"
        p="md"
        component="form"
        aria-label={t('cache.invalidations.formTitle')}
        onSubmit={(event) => {
          event.preventDefault();
          if (scope === 'all') setConfirming(true);
          else submit.mutate();
        }}
      >
        <Stack gap="sm">
          <Title order={4}>{t('cache.invalidations.formTitle')}</Title>
          {distribution === undefined ? (
            <DistributionField
              label={t('cache.distribution')}
              value={chosen}
              choices={choices}
              required
              onChange={(id) => {
                setChosen(id);
                setDomain('');
                submit.reset();
                onDistributionChange?.(id);
              }}
            />
          ) : null}
          <ChoiceField<Scope>
            label={t('cache.invalidations.what')}
            value={scope}
            choices={(['paths', 'patterns', 'tags', 'domain', 'all'] as const).map((s) => ({
              value: s,
              label: t(`cache.invalidations.scopes.${s}`),
              description: t(`cache.invalidations.scopeHints.${s}`),
            }))}
            onChange={(s) => {
              setScope(s);
              submit.reset();
            }}
          />
          {scope === 'paths' ? (
            <Textarea
              label={t('cache.invalidations.paths')}
              description={t('cache.invalidations.pathsHint')}
              autosize
              minRows={3}
              ff="monospace"
              value={paths}
              onChange={(event) => {
                setPaths(event.currentTarget.value);
              }}
            />
          ) : null}
          {scope === 'patterns' ? (
            <Stack gap="xs" role="group" aria-label={t('cache.invalidations.patterns')}>
              {patterns.map((p, i) => (
                <Group key={i} align="flex-start" wrap="nowrap" gap="xs">
                  <div style={{ flex: 1 }}>
                    <PatternField
                      label={t('cache.invalidations.patternN', { n: i + 1 })}
                      value={p}
                      placeholders={{ glob: '/assets/**', prefix: '/assets/', regex: '^/assets/' }}
                      onChange={(next) => {
                        setPatterns(patterns.map((x, j) => (j === i ? next : x)));
                      }}
                    />
                  </div>
                  {patterns.length > 1 ? (
                    <Tooltip label={t('cache.invalidations.removePattern')}>
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        mt={28}
                        aria-label={t('cache.invalidations.removePatternN', { n: i + 1 })}
                        onClick={() => {
                          setPatterns(patterns.filter((_, j) => j !== i));
                        }}
                      >
                        <IconTrash size={16} />
                      </ActionIcon>
                    </Tooltip>
                  ) : null}
                </Group>
              ))}
              <Button
                variant="subtle"
                size="compact-sm"
                leftSection={<IconPlus size={14} />}
                style={{ alignSelf: 'flex-start' }}
                onClick={() => {
                  setPatterns([...patterns, { type: 'glob', pattern: '' }]);
                }}
              >
                {t('cache.invalidations.addPattern')}
              </Button>
            </Stack>
          ) : null}
          {scope === 'tags' ? (
            <TagsInput
              label={t('cache.invalidations.tags')}
              description={t('cache.invalidations.tagsHint')}
              value={tags}
              onChange={setTags}
            />
          ) : null}
          {scope !== 'all' ? domainField : null}
          <Switch
            label={t('cache.invalidations.soft')}
            description={t('cache.invalidations.softHint')}
            checked={soft}
            onChange={(event) => {
              setSoft(event.currentTarget.checked);
            }}
          />
          {submit.isError && !confirming ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, submit.error)}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button
              type="submit"
              color={scope === 'all' ? 'red' : undefined}
              loading={submit.isPending}
              disabled={dist === '' || empty || invalidPattern}
            >
              {scope === 'all'
                ? t('cache.invalidations.purgeAll')
                : soft
                  ? t('cache.invalidations.submitSoft')
                  : t('cache.invalidations.submit')}
            </Button>
          </Group>
        </Stack>
      </Paper>
      {created !== null ? (
        <InvalidationProgress
          key={created.inv.id}
          distribution={created.dist}
          invalidation={created.inv}
        />
      ) : null}
      <ConfirmDialog
        opened={confirming}
        title={t('cache.invalidations.purgeAllTitle')}
        message={t('cache.invalidations.purgeAllConfirm', { id: dist })}
        confirmLabel={t('cache.invalidations.purgeAll')}
        loading={submit.isPending}
        error={submit.isError ? submit.error : undefined}
        onCancel={() => {
          setConfirming(false);
          submit.reset();
        }}
        onConfirm={() => {
          submit.mutate();
        }}
      />
    </Stack>
  );
}

/**
 * The history of the invalidations of a distribution (or of the global
 * purges with ""), newest first, with the state on every node; read again
 * while one is in progress.
 */
export function InvalidationHistory({ distribution }: { distribution: string }) {
  const { t, i18n } = useTranslation();
  const { cache } = useServices();
  const [open, setOpen] = useState<string | null>(null);
  const query = useQuery({
    queryKey: cacheKeys.invalidations(distribution),
    queryFn: () => cache.invalidations(distribution),
    refetchInterval: (q) =>
      (q.state.data ?? []).some((i) => i.state !== 'completed') ? pollMs : false,
  });
  const title =
    distribution === ''
      ? t('cache.invalidations.globalHistory')
      : t('cache.invalidations.history', { id: distribution });
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Title order={4}>{title}</Title>
        {query.isPending ? (
          <Skeleton height={60} />
        ) : query.isError ? (
          <ErrorAlert
            error={query.error}
            title={t('cache.invalidations.loadError')}
            onRetry={() => void query.refetch()}
          />
        ) : query.data.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('cache.invalidations.none')}
          </Text>
        ) : (
          <ScrollTable minWidth={640} label={title}>
            <Table aria-label={title} verticalSpacing={4}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('cache.invalidations.id')}</Table.Th>
                  <Table.Th>{t('cache.invalidations.created')}</Table.Th>
                  <Table.Th>{t('cache.invalidations.scopeColumn')}</Table.Th>
                  <Table.Th>{t('cache.invalidations.actor')}</Table.Th>
                  <Table.Th>{t('cache.invalidations.state')}</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {query.data.map((inv) => (
                  <Fragment key={inv.id}>
                    <Table.Tr>
                      <Table.Td>
                        <Text size="xs" ff="monospace">
                          {inv.id}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs">
                          {new Date(inv.createdAt).toLocaleString(i18n.resolvedLanguage)}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm">{invalidationScope(t, inv.request)}</Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs" ff="monospace">
                          {inv.request.actor ?? '—'}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <InvalidationStateBadge invalidation={inv} />
                      </Table.Td>
                      <Table.Td>
                        <Button
                          size="compact-xs"
                          variant="subtle"
                          aria-expanded={open === inv.id}
                          aria-label={t('cache.invalidations.nodesOf', { id: inv.id })}
                          onClick={() => {
                            setOpen(open === inv.id ? null : inv.id);
                          }}
                        >
                          {t('cache.invalidations.nodes')}
                        </Button>
                      </Table.Td>
                    </Table.Tr>
                    {open === inv.id ? (
                      <Table.Tr>
                        <Table.Td colSpan={6}>
                          <NodeStatusTable
                            nodes={inv.nodes}
                            label={t('cache.invalidations.nodesTable', { id: inv.id })}
                          />
                        </Table.Td>
                      </Table.Tr>
                    ) : null}
                  </Fragment>
                ))}
              </Table.Tbody>
            </Table>
          </ScrollTable>
        )}
      </Stack>
    </Paper>
  );
}
