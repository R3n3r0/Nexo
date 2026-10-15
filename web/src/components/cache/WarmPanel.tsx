import {
  Alert,
  Badge,
  Button,
  Group,
  NumberInput,
  Paper,
  Progress,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
  Title,
} from '@mantine/core';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { PatternField } from '@/components/accessRules/PatternField';
import { ChoiceField } from '@/components/common/ChoiceField';
import { cacheKeys } from '@/lib/cache';
import { formatBytes } from '@/lib/format';
import { patternIssue } from '@/lib/keyPattern';
import { problemMessage } from '@/lib/problem';
import {
  useServices,
  type CacheWarmJob,
  type CacheWarmRequest,
  type Distribution,
  type KeyPattern,
} from '@/services';

import { DistributionField } from './DistributionField';
import { pollMs } from './helpers';
import { NodeStateBadge } from './Invalidations';
import { SizeField } from './fields';

type Source = 'urls' | 'prefix' | 'pattern';

/** The progress of a warm-up on every node, read again until it ends. */
export function WarmProgress({ distribution, job }: { distribution: string; job: CacheWarmJob }) {
  const { t, i18n } = useTranslation();
  const { cache } = useServices();
  const locale = i18n.resolvedLanguage;
  const query = useQuery({
    queryKey: cacheKeys.warmJob(distribution, job.id),
    queryFn: () => cache.warmJob(distribution, job.id),
    initialData: job,
    refetchInterval: (q) => (q.state.data?.state === 'running' ? pollMs : false),
  });
  const w = query.data;
  const total = w.nodes.reduce((s, n) => s + n.total, 0);
  const done = w.nodes.reduce((s, n) => s + n.done, 0);
  const title = t('cache.warm.progressTitle', { id: w.id });
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="xs">
        <Group justify="space-between" wrap="wrap">
          <Text fw={500} size="sm">
            {title}
          </Text>
          <Badge
            variant="light"
            color={w.state === 'completed' ? 'teal' : w.state === 'failed' ? 'red' : 'blue'}
            data-state={w.state}
          >
            {t(`cache.warm.states.${w.state}`)}
          </Badge>
        </Group>
        <Text size="sm" c="dimmed">
          {t('cache.warm.domainOf', { domain: w.request.domain ?? '—' })}
        </Text>
        <Progress
          value={total === 0 ? 100 : (done / total) * 100}
          color={w.state === 'failed' ? 'red' : w.state === 'completed' ? 'teal' : 'blue'}
          aria-label={t('cache.warm.objectsDone', { done, total })}
        />
        <Table aria-label={t('cache.warm.nodesTable', { id: w.id })} verticalSpacing={4}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('cache.nodes.node')}</Table.Th>
              <Table.Th>{t('cache.nodes.state')}</Table.Th>
              <Table.Th ta="right">{t('cache.warm.objects')}</Table.Th>
              <Table.Th ta="right">{t('cache.warm.failed')}</Table.Th>
              <Table.Th ta="right">{t('cache.warm.bytes')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {w.nodes.map((n) => (
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
                <Table.Td ta="right">
                  {t('cache.warm.objectsDone', { done: n.done, total: n.total })}
                </Table.Td>
                <Table.Td ta="right">{n.failed}</Table.Td>
                <Table.Td ta="right">{formatBytes(n.bytes, locale)}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Stack>
    </Paper>
  );
}

export interface WarmPanelProps {
  /** The distribution warmed; undefined: chosen in the form. */
  distribution?: string;
  choices?: readonly Distribution[] | undefined;
  /** The exact domains offered; undefined: typed. */
  domains?: readonly string[] | undefined;
}

/**
 * D-CCH-06: prefetches objects into the cache of a domain (after a deploy
 * of a static site): a list of URLs or paths, a prefix or a pattern, with
 * a bound on the parallel reads and on the bandwidth; the progress is
 * followed node by node.
 */
export function WarmPanel({ distribution, choices, domains }: WarmPanelProps) {
  const { t } = useTranslation();
  const { cache } = useServices();
  const [chosen, setChosen] = useState('');
  const dist = distribution ?? chosen;
  const domainOptions = domains ?? choices?.find((d) => d.id === dist)?.aliases;
  const [source, setSource] = useState<Source>('urls');
  const [urls, setUrls] = useState('');
  const [prefix, setPrefix] = useState('');
  const [pattern, setPattern] = useState<KeyPattern | undefined>({ type: 'glob', pattern: '' });
  const [domain, setDomain] = useState('');
  const [concurrency, setConcurrency] = useState<number | undefined>(undefined);
  const [bandwidth, setBandwidth] = useState<string | undefined>(undefined);
  const [job, setJob] = useState<{ dist: string; job: CacheWarmJob } | null>(null);

  const lines = urls
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l !== '');
  const request: CacheWarmRequest = {
    ...(domain.trim() !== '' ? { domain: domain.trim() } : {}),
    ...(source === 'urls'
      ? {
          urls: lines.filter((l) => /^https?:\/\//i.test(l)),
          paths: lines.filter((l) => !/^https?:\/\//i.test(l)),
        }
      : {}),
    ...(source === 'prefix' ? { prefix: prefix.trim() } : {}),
    ...(source === 'pattern' && pattern !== undefined ? { pattern } : {}),
    ...(concurrency !== undefined ? { concurrency } : {}),
    ...(bandwidth !== undefined ? { bandwidth } : {}),
  };
  if (request.urls?.length === 0) delete request.urls;
  if (request.paths?.length === 0) delete request.paths;
  const empty =
    (source === 'urls' && lines.length === 0) ||
    (source === 'prefix' && prefix.trim() === '') ||
    (source === 'pattern' && (pattern === undefined || pattern.pattern.trim() === ''));
  const invalid =
    source === 'pattern' && pattern !== undefined && patternIssue(pattern) !== undefined;

  const start = useMutation({
    mutationFn: () => cache.warm(dist, request),
    onSuccess: (created) => {
      setJob({ dist, job: created });
    },
  });

  return (
    <Stack gap="md">
      <Paper
        withBorder
        radius="md"
        p="md"
        component="form"
        aria-label={t('cache.warm.title')}
        onSubmit={(event) => {
          event.preventDefault();
          start.mutate();
        }}
      >
        <Stack gap="sm">
          <Title order={4}>{t('cache.warm.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('cache.warm.hint')}
          </Text>
          {distribution === undefined ? (
            <DistributionField
              label={t('cache.distribution')}
              value={chosen}
              choices={choices}
              required
              onChange={(id) => {
                setChosen(id);
                setDomain('');
                start.reset();
              }}
            />
          ) : null}
          {domainOptions === undefined ? (
            <TextInput
              label={t('cache.warm.domain')}
              description={t('cache.warm.domainHint')}
              value={domain}
              onChange={(event) => {
                setDomain(event.currentTarget.value);
              }}
            />
          ) : (
            <Select
              label={t('cache.warm.domain')}
              description={t('cache.warm.domainHint')}
              clearable
              data={domainOptions.filter((d) => !d.startsWith('*.'))}
              value={domain === '' ? null : domain}
              onChange={(v) => {
                setDomain(v ?? '');
              }}
            />
          )}
          <ChoiceField<Source>
            label={t('cache.warm.source')}
            value={source}
            choices={(['urls', 'prefix', 'pattern'] as const).map((s) => ({
              value: s,
              label: t(`cache.warm.sources.${s}`),
            }))}
            onChange={(s) => {
              setSource(s);
              start.reset();
            }}
          />
          {source === 'urls' ? (
            <Textarea
              label={t('cache.warm.urls')}
              description={t('cache.warm.urlsHint')}
              autosize
              minRows={4}
              ff="monospace"
              value={urls}
              onChange={(event) => {
                setUrls(event.currentTarget.value);
              }}
            />
          ) : source === 'prefix' ? (
            <TextInput
              label={t('cache.warm.prefix')}
              placeholder="/assets/"
              ff="monospace"
              value={prefix}
              onChange={(event) => {
                setPrefix(event.currentTarget.value);
              }}
            />
          ) : (
            <PatternField
              label={t('cache.warm.pattern')}
              value={pattern}
              placeholders={{ glob: '/assets/**', prefix: '/assets/', regex: '^/assets/' }}
              onChange={setPattern}
            />
          )}
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
            <NumberInput
              label={t('cache.warm.concurrency')}
              description={t('cache.warm.concurrencyHint')}
              min={1}
              max={64}
              allowDecimal={false}
              value={concurrency ?? ''}
              onChange={(n) => {
                setConcurrency(typeof n === 'number' ? n : undefined);
              }}
            />
            <SizeField
              label={t('cache.warm.bandwidth')}
              description={t('cache.warm.bandwidthHint')}
              value={bandwidth}
              onChange={setBandwidth}
            />
          </SimpleGrid>
          {start.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, start.error)}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button
              type="submit"
              loading={start.isPending}
              disabled={dist === '' || empty || invalid}
            >
              {t('cache.warm.start')}
            </Button>
          </Group>
        </Stack>
      </Paper>
      {job !== null ? (
        <WarmProgress key={job.job.id} distribution={job.dist} job={job.job} />
      ) : null}
    </Stack>
  );
}
