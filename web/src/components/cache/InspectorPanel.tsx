import {
  Alert,
  Badge,
  Button,
  Code,
  CopyButton,
  Group,
  Paper,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  TagsInput,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconSearch } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { ErrorAlert } from '@/components/iam/common';
import { formatDuration } from '@/lib/cache';
import { formatBytes } from '@/lib/format';
import {
  useServices,
  type CacheInspectInput,
  type CacheInspection,
  type CacheInspectionNode,
  type Distribution,
} from '@/services';

import { DistributionField } from './DistributionField';
import { EffectivePolicyView } from './EffectivePolicyView';
import { ScrollTable } from './ScrollTable';

function EntryCell({ node }: { node: CacheInspectionNode }) {
  const { t } = useTranslation();
  const e = node.entry;
  if (node.error !== undefined && node.error !== '') {
    return (
      <Text size="sm" c="red">
        {node.error}
      </Text>
    );
  }
  if (!e.present) {
    return (
      <Badge variant="light" color="gray" data-present="false">
        {t('cache.inspector.notCached')}
      </Badge>
    );
  }
  return (
    <Group gap={4} wrap="wrap">
      <Badge variant="light" color={e.stale === true ? 'yellow' : 'teal'} data-present="true">
        {e.tier === 'disk' ? t('cache.inspector.cachedDisk') : t('cache.inspector.cachedMemory')}
      </Badge>
      {e.kind !== undefined ? (
        <Badge variant="outline" color="gray" size="sm">
          {t(`cache.inspector.kinds.${e.kind}`)}
        </Badge>
      ) : null}
      {e.stale === true ? (
        <Badge variant="light" color="yellow" size="sm">
          {t('cache.inspector.stale')}
        </Badge>
      ) : null}
    </Group>
  );
}

/** The entry of the URL on every node of the cluster. */
function NodesTable({ inspection }: { inspection: CacheInspection }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  return (
    <ScrollTable minWidth={640} label={t('cache.inspector.nodesTable')}>
      <Table aria-label={t('cache.inspector.nodesTable')} verticalSpacing={4}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t('cache.nodes.node')}</Table.Th>
            <Table.Th>{t('cache.inspector.entry')}</Table.Th>
            <Table.Th ta="right">{t('cache.inspector.age')}</Table.Th>
            <Table.Th ta="right">{t('cache.inspector.expires')}</Table.Th>
            <Table.Th ta="right">{t('cache.inspector.size')}</Table.Th>
            <Table.Th>{t('cache.inspector.encodings')}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {inspection.nodes.map((n) => {
            const e = n.entry;
            return (
              <Table.Tr key={n.node} data-node={n.node}>
                <Table.Td>
                  <Group gap={4} wrap="nowrap">
                    <Text size="sm" ff="monospace">
                      {n.node}
                    </Text>
                    {!n.online ? (
                      <Badge size="xs" color="red" variant="light">
                        {t('cache.nodes.offline')}
                      </Badge>
                    ) : null}
                    {n.owner === true ? (
                      <Badge size="xs" color="grape" variant="light">
                        {t('cache.inspector.owner')}
                      </Badge>
                    ) : null}
                  </Group>
                </Table.Td>
                <Table.Td>
                  <EntryCell node={n} />
                </Table.Td>
                <Table.Td ta="right">{e.present ? formatDuration(e.ageSeconds) : '—'}</Table.Td>
                <Table.Td ta="right">
                  {!e.present
                    ? '—'
                    : e.expiresInSeconds >= 0
                      ? t('cache.inspector.freshFor', {
                          duration: formatDuration(e.expiresInSeconds),
                        })
                      : t('cache.inspector.staleSince', {
                          duration: formatDuration(-e.expiresInSeconds),
                        })}
                </Table.Td>
                <Table.Td ta="right">{e.present ? formatBytes(e.sizeBytes, locale) : '—'}</Table.Td>
                <Table.Td>
                  <Text size="xs" ff="monospace">
                    {e.encodings.length === 0 ? '—' : e.encodings.join(', ')}
                    {e.blocks !== undefined && e.blocks > 0
                      ? ` · ${t('cache.inspector.blocks', { n: e.blocks })}`
                      : ''}
                  </Text>
                </Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
    </ScrollTable>
  );
}

/** The result of an inspection: the request, the key, the policy, the nodes. */
export function InspectionResult({ inspection }: { inspection: CacheInspection }) {
  const { t } = useTranslation();
  const facts: { label: string; value: string }[] = [
    { label: t('cache.inspector.distribution'), value: inspection.distribution ?? '—' },
    { label: t('cache.inspector.host'), value: inspection.host },
    { label: t('cache.inspector.path'), value: inspection.path },
    {
      label: t('cache.inspector.object'),
      value:
        inspection.bucket !== undefined
          ? `${inspection.bucket}/${inspection.key ?? ''}`
          : (inspection.key ?? '—'),
    },
    { label: t('cache.inspector.mode'), value: t(`cache.modes.${inspection.mode}`) },
  ];
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('cache.inspector.result')}
    >
      <Stack gap="sm">
        <Title order={4}>{t('cache.inspector.result')}</Title>
        <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="xs">
          {facts.map((f) => (
            <Stack key={f.label} gap={0}>
              <Text size="xs" c="dimmed">
                {f.label}
              </Text>
              <Text size="sm" ff="monospace" style={{ wordBreak: 'break-all' }}>
                {f.value}
              </Text>
            </Stack>
          ))}
        </SimpleGrid>
        {inspection.cacheKey !== undefined && inspection.cacheKey !== '' ? (
          <Stack gap={2}>
            <Text size="xs" c="dimmed" id="cache-key-label">
              {t('cache.inspector.cacheKey')}
            </Text>
            <Group gap="xs" wrap="nowrap" align="flex-start">
              <Code
                block
                aria-labelledby="cache-key-label"
                style={{ flex: 1, wordBreak: 'break-all', whiteSpace: 'pre-wrap' }}
              >
                {inspection.cacheKey}
              </Code>
              <CopyButton value={inspection.cacheKey}>
                {({ copied, copy }) => (
                  <Button size="compact-xs" variant="default" onClick={copy}>
                    {copied ? t('common.copied') : t('common.copy')}
                  </Button>
                )}
              </CopyButton>
            </Group>
          </Stack>
        ) : null}
        {inspection.bypass ? (
          <Alert color="yellow" variant="light" title={t('cache.inspector.bypass')}>
            {inspection.reason ?? t('cache.inspector.bypassUnknown')}
          </Alert>
        ) : null}
        <Title order={5}>{t('cache.inspector.policy')}</Title>
        <EffectivePolicyView
          effective={inspection.policy}
          label={t('cache.inspector.effectivePolicy')}
        />
        <Title order={5}>{t('cache.inspector.nodes')}</Title>
        <NodesTable inspection={inspection} />
      </Stack>
    </Paper>
  );
}

export interface InspectorPanelProps {
  /** The distribution of the paths; undefined: chosen in the form. */
  distribution?: string;
  choices?: readonly Distribution[] | undefined;
}

/**
 * D-CCH-05: the URL inspector. For a URL (or a path of a distribution)
 * the cache key computed for it, the effective policy with the level of
 * every setting, the reason of a bypass and the entry on every node.
 */
export function InspectorPanel({ distribution, choices }: InspectorPanelProps) {
  const { t } = useTranslation();
  const { cache } = useServices();
  const [url, setUrl] = useState('');
  const [chosen, setChosen] = useState('');
  const [headers, setHeaders] = useState<string[]>([]);
  const [submitted, setSubmitted] = useState<CacheInspectInput | null>(null);
  const dist = distribution ?? chosen;
  const query = useQuery({
    queryKey: ['cache', 'inspect', submitted] as const,
    queryFn: () => cache.inspect(submitted ?? { url: '' }),
    enabled: submitted !== null,
    retry: false,
  });
  const isPath = url.trim().startsWith('/');
  return (
    <Stack gap="md">
      <Paper
        withBorder
        radius="md"
        p="md"
        component="form"
        aria-label={t('cache.inspector.title')}
        onSubmit={(event) => {
          event.preventDefault();
          const next: CacheInspectInput = {
            url: url.trim(),
            ...(dist !== '' ? { distribution: dist } : {}),
            ...(headers.length > 0 ? { headers } : {}),
          };
          // The same URL inspected again: read the entries again.
          if (JSON.stringify(next) === JSON.stringify(submitted)) void query.refetch();
          else setSubmitted(next);
        }}
      >
        <Stack gap="sm">
          <Title order={4}>{t('cache.inspector.title')}</Title>
          <TextInput
            label={t('cache.inspector.url')}
            description={t('cache.inspector.urlHint')}
            placeholder="https://www.example.com/assets/app.js"
            ff="monospace"
            value={url}
            onChange={(event) => {
              setUrl(event.currentTarget.value);
            }}
          />
          {distribution === undefined ? (
            <DistributionField
              label={t('cache.distribution')}
              description={t('cache.inspector.distributionHint')}
              value={chosen}
              choices={choices}
              required={isPath}
              onChange={setChosen}
            />
          ) : null}
          <TagsInput
            label={t('cache.inspector.headers')}
            description={t('cache.inspector.headersHint')}
            placeholder="Accept-Encoding: br"
            value={headers}
            onChange={setHeaders}
          />
          <Group justify="flex-end">
            <Button
              type="submit"
              leftSection={<IconSearch size={16} />}
              loading={query.isFetching}
              disabled={url.trim() === '' || (isPath && dist === '')}
            >
              {t('cache.inspector.inspect')}
            </Button>
          </Group>
        </Stack>
      </Paper>
      {submitted === null ? null : query.isPending ? (
        <Skeleton height={200} />
      ) : query.isError ? (
        <ErrorAlert error={query.error} title={t('cache.inspector.error')} />
      ) : (
        <InspectionResult inspection={query.data} />
      )}
    </Stack>
  );
}
