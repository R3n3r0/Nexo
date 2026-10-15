import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Modal,
  Paper,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import {
  IconAlertTriangle,
  IconArrowDown,
  IconArrowUp,
  IconDeviceFloppy,
  IconEdit,
  IconPlus,
  IconRefresh,
  IconTrash,
} from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router';

import { CacheActions, usePermissions } from '@/auth/usePermissions';
import { PatternField } from '@/components/accessRules/PatternField';
import { EffectivePolicyView, LevelBadge } from '@/components/cache/EffectivePolicyView';
import { InvalidationForm, InvalidationHistory } from '@/components/cache/Invalidations';
import { settingsSummary, typedPattern } from '@/components/cache/helpers';
import { SettingsFields } from '@/components/cache/SettingsFields';
import { StatsView } from '@/components/cache/StatsView';
import { WarmPanel } from '@/components/cache/WarmPanel';
import { WebhooksPanel } from '@/components/cache/WebhooksPanel';
import { ErrorAlert } from '@/components/iam/common';
import {
  cacheKeys,
  cacheableRequests,
  combineHostStats,
  domainOverrideIndex,
  isEmptySettings,
  pruneSettings,
  formatRatio,
  isDomainOf,
  normalizeCacheHost,
  resolveLevels,
  specOf,
  validateSpec,
  type LevelSettings,
} from '@/lib/cache';
import { formatBytes } from '@/lib/format';
import { patternIssue } from '@/lib/keyPattern';
import { problemMessage } from '@/lib/problem';
import {
  isApiError,
  useServices,
  type CacheBehaviorSettings,
  type CacheDomainSettings,
  type CachePolicy,
  type CacheSettings,
  type Distribution,
  type DistributionCache,
  type KeyPattern,
} from '@/services';
import { ScrollTable } from '@/components/cache/ScrollTable';

const sections = ['settings', 'statistics', 'invalidations', 'warm', 'webhooks'] as const;
type Section = (typeof sections)[number];

/** The editable document: without the server fields. */
function editable(c: DistributionCache): DistributionCache {
  const { etag: _etag, updatedAt: _updatedAt, ...rest } = c;
  return rest;
}

function same(a: DistributionCache, b: DistributionCache): boolean {
  return JSON.stringify(editable(a)) === JSON.stringify(editable(b));
}

/** The document to save: empty levels dropped, the domains and paths without empty overrides. */
function normalized(c: DistributionCache): DistributionCache {
  const out: DistributionCache = { ...editable(c), cache: pruneSettings(c.cache) };
  if (c.domains !== undefined) {
    out.domains = c.domains.map((d) => ({ host: d.host, cache: pruneSettings(d.cache) }));
  }
  if (c.behaviors !== undefined) {
    out.behaviors = c.behaviors.map((b) => ({
      pathPattern: b.pathPattern,
      cache: pruneSettings(b.cache),
    }));
  }
  return out;
}

function Section({
  title,
  actions,
  children,
}: {
  title: string;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Title order={4}>{title}</Title>
          {actions}
        </Group>
        {children}
      </Stack>
    </Paper>
  );
}

interface DomainEdit {
  /** Index in domains, -1 for a new override. */
  index: number;
  host: string;
  cache: CacheSettings;
  /** The host is typed (a host below a wildcard alias). */
  free: boolean;
}

interface BehaviorEdit {
  index: number;
  pathPattern: KeyPattern | undefined;
  cache: CacheSettings;
}

/**
 * D-CCH-02: the cache tab of a distribution. Its settings (defaults, the
 * overrides per domain and per path, warm-on-write) are a document of
 * their own, saved at once with If-Match on its etag; the domains table
 * shows for every domain the effective state and policy (with the level
 * that sets it), the quota, the space used and the hit ratio. The other
 * sections are the statistics per domain (D-CCH-08), the invalidations
 * (D-CCH-04), the warm-ups (D-CCH-06) and the purge webhooks (D-CCH-07).
 */
export function DistributionCacheTab({ distribution }: { distribution: Distribution }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { cache } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const canManage = perms.can(CacheActions.manage);
  const canInvalidate = perms.can(CacheActions.invalidate);
  const id = distribution.id;
  const [params, setParams] = useSearchParams();
  const visible = sections.filter((s) =>
    s === 'invalidations' || s === 'warm' ? canInvalidate : canManage,
  );
  const requested = params.get('section') as Section | null;
  const section: Section | undefined =
    requested !== null && visible.includes(requested) ? requested : visible[0];

  const key = cacheKeys.distribution(id);
  const config = useQuery({
    queryKey: key,
    queryFn: () => cache.getDistribution(id),
    enabled: canManage,
  });
  const policies = useQuery({
    queryKey: cacheKeys.policies,
    queryFn: () => cache.listPolicies(),
    enabled: canManage,
  });
  const global = useQuery({
    queryKey: cacheKeys.settings,
    queryFn: () => cache.getSettings(),
    enabled: canManage,
  });
  const stats = useQuery({
    queryKey: cacheKeys.stats(id, ''),
    queryFn: () => cache.stats({ distribution: id, top: 10 }),
    enabled: canManage,
    refetchInterval: 15_000,
  });
  const [draft, setDraft] = useState<DistributionCache | null>(null);
  const [domainEdit, setDomainEdit] = useState<DomainEdit | null>(null);
  const [behaviorEdit, setBehaviorEdit] = useState<BehaviorEdit | null>(null);
  const [testHost, setTestHost] = useState('');
  const [testPath, setTestPath] = useState('/');
  const [tested, setTested] = useState<{ host: string; path: string } | null>(null);

  const base = config.data;
  const current = draft ?? base;
  const dirty = draft !== null && base !== undefined && !same(draft, base);

  const save = useMutation({
    mutationFn: (c: DistributionCache) =>
      cache.updateDistribution({ ...normalized(c), distribution: id }, base?.etag),
    onSuccess: async (stored) => {
      queryClient.setQueryData(key, stored);
      setDraft(null);
      await queryClient.invalidateQueries({ queryKey: cacheKeys.webhooks(id) });
      await queryClient.invalidateQueries({
        queryKey: ['cache', 'distribution', id, 'effective'],
      });
      notifications.show({ color: 'green', message: t('cache.tab.saved') });
    },
  });
  const conflict = isApiError(save.error) && save.error.isPreconditionFailed;
  const effective = useQuery({
    queryKey: cacheKeys.effective(id, tested?.host ?? '', tested?.path ?? ''),
    queryFn: () => cache.effective(id, tested ?? {}),
    enabled: tested !== null && canManage,
    retry: false,
  });

  const change = (next: DistributionCache) => {
    save.reset();
    setDraft(next);
  };

  const policyList: CachePolicy[] = policies.data ?? [];
  const policyNames = policyList.map((p) => p.name);
  const policyMap = new Map(policyList.map((p) => [p.name, p]));
  const aliases = distribution.aliases;
  const webhookIds = (current?.webhooks ?? []).map((w) => w.id);

  const levelsFor = (host: string): LevelSettings[] => {
    const levels: LevelSettings[] = [{ level: 'global', settings: global.data?.cache }];
    if (current !== undefined) {
      levels.push({ level: 'distribution', settings: current.cache });
      const i = domainOverrideIndex(current.domains, host);
      if (i >= 0) levels.push({ level: 'domain', settings: current.domains?.[i]?.cache });
    }
    return levels;
  };
  const inheritedOf = (levels: LevelSettings[]) => {
    const eff = resolveLevels(levels, policyMap);
    return { enabled: eff.enabled, policy: eff.policyName };
  };

  const sectionControl = (
    <SegmentedControl
      aria-label={t('cache.tab.sections')}
      value={section ?? ''}
      data={visible.map((s) => ({ value: s, label: t(`cache.tab.sectionNames.${s}`) }))}
      onChange={(value) => {
        const next = new URLSearchParams(params);
        next.set('section', value);
        setParams(next, { replace: true });
      }}
      style={{ alignSelf: 'flex-start', maxWidth: '100%' }}
    />
  );

  if (section === undefined) {
    return (
      <Alert color="gray" variant="light">
        {t('cache.tab.forbidden')}
      </Alert>
    );
  }

  if (section === 'invalidations') {
    return (
      <Stack gap="md">
        {sectionControl}
        <InvalidationForm distribution={id} domains={aliases} />
        <InvalidationHistory distribution={id} />
      </Stack>
    );
  }
  if (section === 'warm') {
    return (
      <Stack gap="md">
        {sectionControl}
        <WarmPanel distribution={id} domains={aliases} />
      </Stack>
    );
  }
  if (section === 'statistics') {
    return (
      <Stack gap="md">
        {sectionControl}
        {stats.isPending ? (
          <Skeleton height={240} />
        ) : stats.isError ? (
          <ErrorAlert error={stats.error} title={t('cache.stats.loadError')} />
        ) : (
          <StatsView stats={stats.data} global={false} />
        )}
      </Stack>
    );
  }
  if (section === 'webhooks') {
    return (
      <Stack gap="md">
        {sectionControl}
        <WebhooksPanel
          distribution={id}
          domains={aliases}
          locked={dirty}
          onChanged={() => {
            void queryClient.invalidateQueries({ queryKey: key });
          }}
        />
      </Stack>
    );
  }

  // Settings.
  if (config.isError) {
    return (
      <Stack gap="md">
        {sectionControl}
        <ErrorAlert error={config.error} title={t('cache.tab.loadError')} />
      </Stack>
    );
  }
  if (current === undefined) {
    return (
      <Stack gap="md">
        {sectionControl}
        <Skeleton height={240} />
      </Stack>
    );
  }

  const domains = current.domains ?? [];
  const rows = [...aliases, ...domains.map((d) => d.host).filter((h) => !aliases.includes(h))];
  const behaviors = current.behaviors ?? [];
  const warmOnWrite = current.warmOnWrite ?? [];
  const invalidLevel =
    validateSpec(specOf(current.cache)).length > 0 ||
    domains.some((d) => validateSpec(specOf(d.cache)).length > 0) ||
    behaviors.some((b) => validateSpec(specOf(b.cache)).length > 0) ||
    warmOnWrite.some((p) => patternIssue(p) !== undefined);

  const setDomains = (next: CacheDomainSettings[]) => {
    change({ ...current, domains: next });
  };
  const setBehaviors = (next: CacheBehaviorSettings[]) => {
    change({ ...current, behaviors: next });
  };

  return (
    <Stack gap="md">
      {sectionControl}
      {dirty || conflict || save.isError ? (
        <Paper
          withBorder
          radius="md"
          p="sm"
          role="region"
          aria-label={t('cache.tab.unsavedTitle')}
          style={{
            position: 'sticky',
            top: 64,
            zIndex: 5,
            background: 'var(--mantine-color-body)',
          }}
        >
          <Stack gap="xs">
            {conflict ? (
              <Alert
                color="yellow"
                variant="light"
                icon={<IconAlertTriangle size={18} />}
                title={t('cache.tab.conflictTitle')}
              >
                {t('cache.tab.conflict')}
              </Alert>
            ) : save.isError ? (
              <Alert color="red" variant="light" role="alert">
                {problemMessage(t, save.error)}
              </Alert>
            ) : null}
            <Group justify="space-between" wrap="wrap">
              <Text size="sm" fw={500}>
                {dirty ? t('cache.tab.unsaved') : ''}
              </Text>
              <Group gap="xs">
                <Button
                  variant="default"
                  leftSection={conflict ? <IconRefresh size={16} /> : undefined}
                  onClick={() => {
                    setDraft(null);
                    save.reset();
                    if (conflict) void config.refetch();
                  }}
                >
                  {conflict ? t('cache.tab.reload') : t('cache.tab.discard')}
                </Button>
                {!conflict ? (
                  <Button
                    leftSection={<IconDeviceFloppy size={16} />}
                    loading={save.isPending}
                    disabled={!dirty || invalidLevel}
                    onClick={() => {
                      if (draft !== null) save.mutate(draft);
                    }}
                  >
                    {t('cache.tab.save')}
                  </Button>
                ) : null}
              </Group>
            </Group>
          </Stack>
        </Paper>
      ) : null}

      <Section title={t('cache.tab.defaults')}>
        <Text size="sm" c="dimmed">
          {t('cache.tab.defaultsHint')}
        </Text>
        <SettingsFields
          level="distribution"
          settings={current.cache}
          policies={policyNames}
          inherited={inheritedOf([{ level: 'global', settings: global.data?.cache }])}
          onChange={(cacheSettings) => {
            change({ ...current, cache: cacheSettings });
          }}
        />
      </Section>

      <Section
        title={t('cache.tab.domains')}
        actions={
          aliases.some((a) => a.startsWith('*.')) ? (
            <Button
              size="xs"
              variant="light"
              leftSection={<IconPlus size={14} />}
              onClick={() => {
                setDomainEdit({ index: -1, host: '', cache: {}, free: true });
              }}
            >
              {t('cache.tab.addHost')}
            </Button>
          ) : undefined
        }
      >
        <ScrollTable minWidth={820} label={t('cache.tab.domains')}>
          <Table aria-label={t('cache.tab.domains')} verticalSpacing={6}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('cache.tab.domain')}</Table.Th>
                <Table.Th>{t('cache.tab.cache')}</Table.Th>
                <Table.Th>{t('cache.tab.policy')}</Table.Th>
                <Table.Th>{t('cache.tab.quota')}</Table.Th>
                <Table.Th ta="right">{t('cache.tab.usage')}</Table.Th>
                <Table.Th ta="right">{t('cache.tab.hitRatio')}</Table.Th>
                <Table.Th>{t('cache.tab.override')}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((host) => {
                const index = domains.findIndex((d) => d.host === host);
                const override = index >= 0 ? domains[index] : undefined;
                const eff = resolveLevels(levelsFor(host), policyMap);
                const hs = combineHostStats(stats.data?.hosts ?? [], host);
                return (
                  <Table.Tr key={host} data-domain={host}>
                    <Table.Td>
                      <Text size="sm" ff="monospace">
                        {host}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="nowrap">
                        <Badge
                          variant="light"
                          color={eff.enabled ? 'teal' : 'gray'}
                          data-enabled={eff.enabled ? 'true' : 'false'}
                        >
                          {eff.enabled ? t('cache.tab.on') : t('cache.tab.off')}
                        </Badge>
                        <LevelBadge level={eff.enabledLevel} />
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="nowrap">
                        <Text size="sm" ff="monospace" data-policy={eff.policyName}>
                          {eff.policyName}
                        </Text>
                        <LevelBadge level={eff.policyLevel} />
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">
                        {override?.cache.quota !== undefined
                          ? String(override.cache.quota)
                          : current.cache.quota !== undefined
                            ? t('cache.tab.sharedQuota', { quota: String(current.cache.quota) })
                            : '—'}
                      </Text>
                    </Table.Td>
                    <Table.Td ta="right">
                      {formatBytes(hs.memoryBytes + hs.diskBytes, locale)}
                    </Table.Td>
                    <Table.Td ta="right" data-column="hit-ratio">
                      {formatRatio(hs.hitRatio, cacheableRequests(hs.requests), locale)}
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="nowrap">
                        {override !== undefined ? (
                          <Tooltip label={settingsSummary(t, override.cache)} multiline w={260}>
                            <Badge variant="outline" color="teal">
                              {t('cache.tab.overridden')}
                            </Badge>
                          </Tooltip>
                        ) : (
                          <Text size="xs" c="dimmed">
                            {t('cache.tab.inherits')}
                          </Text>
                        )}
                        <Tooltip
                          label={
                            override !== undefined
                              ? t('cache.tab.editOverride')
                              : t('cache.tab.addOverride')
                          }
                        >
                          <ActionIcon
                            variant="subtle"
                            aria-label={
                              override !== undefined
                                ? t('cache.tab.editOverrideOf', { host })
                                : t('cache.tab.addOverrideOf', { host })
                            }
                            onClick={() => {
                              setDomainEdit({
                                index,
                                host,
                                cache: structuredClone(override?.cache ?? {}),
                                free: false,
                              });
                            }}
                          >
                            <IconEdit size={16} />
                          </ActionIcon>
                        </Tooltip>
                        {override !== undefined ? (
                          <Tooltip label={t('cache.tab.removeOverride')}>
                            <ActionIcon
                              variant="subtle"
                              color="red"
                              aria-label={t('cache.tab.removeOverrideOf', { host })}
                              onClick={() => {
                                setDomains(domains.filter((_, j) => j !== index));
                              }}
                            >
                              <IconTrash size={16} />
                            </ActionIcon>
                          </Tooltip>
                        ) : null}
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                );
              })}
            </Table.Tbody>
          </Table>
        </ScrollTable>
      </Section>

      <Section
        title={t('cache.tab.behaviors')}
        actions={
          <Button
            size="xs"
            variant="light"
            leftSection={<IconPlus size={14} />}
            onClick={() => {
              setBehaviorEdit({
                index: -1,
                pathPattern: { type: 'glob', pattern: '' },
                cache: {},
              });
            }}
          >
            {t('cache.tab.addBehavior')}
          </Button>
        }
      >
        <Text size="sm" c="dimmed">
          {t('cache.tab.behaviorsHint')}
        </Text>
        {behaviors.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('cache.tab.noBehaviors')}
          </Text>
        ) : (
          <Table aria-label={t('cache.tab.behaviors')} verticalSpacing={4}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>#</Table.Th>
                <Table.Th>{t('cache.tab.pattern')}</Table.Th>
                <Table.Th>{t('cache.tab.settings')}</Table.Th>
                <Table.Th>{t('common.actions')}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {behaviors.map((b, i) => (
                <Table.Tr key={`${String(i)}-${b.pathPattern.type}-${b.pathPattern.pattern}`}>
                  <Table.Td>{i + 1}</Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace">
                      {typedPattern(b.pathPattern)}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{settingsSummary(t, b.cache)}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={2} wrap="nowrap">
                      <ActionIcon
                        variant="subtle"
                        disabled={i === 0}
                        aria-label={t('cache.tab.moveUp', { n: i + 1 })}
                        onClick={() => {
                          const next = [...behaviors];
                          [next[i - 1], next[i]] = [
                            next[i] as CacheBehaviorSettings,
                            next[i - 1] as CacheBehaviorSettings,
                          ];
                          setBehaviors(next);
                        }}
                      >
                        <IconArrowUp size={16} />
                      </ActionIcon>
                      <ActionIcon
                        variant="subtle"
                        disabled={i === behaviors.length - 1}
                        aria-label={t('cache.tab.moveDown', { n: i + 1 })}
                        onClick={() => {
                          const next = [...behaviors];
                          [next[i + 1], next[i]] = [
                            next[i] as CacheBehaviorSettings,
                            next[i + 1] as CacheBehaviorSettings,
                          ];
                          setBehaviors(next);
                        }}
                      >
                        <IconArrowDown size={16} />
                      </ActionIcon>
                      <ActionIcon
                        variant="subtle"
                        aria-label={t('cache.tab.editBehavior', { n: i + 1 })}
                        onClick={() => {
                          setBehaviorEdit({
                            index: i,
                            pathPattern: b.pathPattern,
                            cache: structuredClone(b.cache),
                          });
                        }}
                      >
                        <IconEdit size={16} />
                      </ActionIcon>
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        aria-label={t('cache.tab.removeBehavior', { n: i + 1 })}
                        onClick={() => {
                          setBehaviors(behaviors.filter((_, j) => j !== i));
                        }}
                      >
                        <IconTrash size={16} />
                      </ActionIcon>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Section>

      <Section
        title={t('cache.tab.warmOnWrite')}
        actions={
          <Button
            size="xs"
            variant="light"
            leftSection={<IconPlus size={14} />}
            onClick={() => {
              change({ ...current, warmOnWrite: [...warmOnWrite, { type: 'glob', pattern: '' }] });
            }}
          >
            {t('cache.tab.addWarmPattern')}
          </Button>
        }
      >
        <Text size="sm" c="dimmed">
          {t('cache.tab.warmOnWriteHint')}
        </Text>
        {warmOnWrite.map((p, i) => (
          <Group key={i} align="flex-start" wrap="nowrap" gap="xs">
            <div style={{ flex: 1 }}>
              <PatternField
                label={t('cache.tab.warmPatternN', { n: i + 1 })}
                value={p}
                onChange={(next) => {
                  if (next === undefined) return;
                  change({
                    ...current,
                    warmOnWrite: warmOnWrite.map((x, j) => (j === i ? next : x)),
                  });
                }}
              />
            </div>
            <ActionIcon
              variant="subtle"
              color="red"
              mt={28}
              aria-label={t('cache.tab.removeWarmPattern', { n: i + 1 })}
              onClick={() => {
                change({ ...current, warmOnWrite: warmOnWrite.filter((_, j) => j !== i) });
              }}
            >
              <IconTrash size={16} />
            </ActionIcon>
          </Group>
        ))}
      </Section>

      <Section title={t('cache.tab.effectiveTitle')}>
        <Text size="sm" c="dimmed">
          {dirty ? t('cache.tab.effectiveSaved') : t('cache.tab.effectiveHint')}
        </Text>
        <Group
          align="flex-end"
          wrap="wrap"
          gap="sm"
          component="form"
          onSubmit={(event) => {
            event.preventDefault();
            setTested({ host: testHost, path: testPath === '' ? '/' : testPath });
          }}
        >
          <Select
            label={t('cache.tab.testHost')}
            data={aliases.filter((a) => !a.startsWith('*.'))}
            value={testHost === '' ? null : testHost}
            onChange={(v) => {
              setTestHost(v ?? '');
            }}
            searchable
            style={{ minWidth: 220 }}
          />
          <TextInput
            label={t('cache.tab.testPath')}
            ff="monospace"
            value={testPath}
            onChange={(event) => {
              setTestPath(event.currentTarget.value);
            }}
          />
          <Button type="submit" variant="default" disabled={testHost === ''}>
            {t('cache.tab.testSubmit')}
          </Button>
        </Group>
        {tested === null ? null : effective.isPending ? (
          <Skeleton height={120} />
        ) : effective.isError ? (
          <ErrorAlert error={effective.error} />
        ) : (
          <EffectivePolicyView
            effective={effective.data}
            label={t('cache.tab.effectiveOf', { host: tested.host, path: tested.path })}
          />
        )}
      </Section>

      {domainEdit !== null ? (
        <Modal
          opened
          size="xl"
          onClose={() => {
            setDomainEdit(null);
          }}
          title={
            domainEdit.free
              ? t('cache.tab.addHostTitle')
              : t('cache.tab.overrideTitle', { host: domainEdit.host })
          }
        >
          <Stack gap="sm">
            {domainEdit.free ? (
              <TextInput
                label={t('cache.tab.host')}
                description={t('cache.tab.hostHint')}
                value={domainEdit.host}
                error={
                  domainEdit.host !== '' &&
                  !isDomainOf(aliases, normalizeCacheHost(domainEdit.host))
                    ? t('cache.tab.notADomain')
                    : undefined
                }
                onChange={(event) => {
                  setDomainEdit({ ...domainEdit, host: event.currentTarget.value });
                }}
              />
            ) : null}
            <SettingsFields
              level="domain"
              settings={domainEdit.cache}
              policies={policyNames}
              webhooks={webhookIds}
              inherited={inheritedOf([
                { level: 'global', settings: global.data?.cache },
                { level: 'distribution', settings: current.cache },
              ])}
              onChange={(c) => {
                setDomainEdit({ ...domainEdit, cache: c });
              }}
            />
            <Group justify="flex-end" gap="xs">
              <Button
                variant="default"
                onClick={() => {
                  setDomainEdit(null);
                }}
              >
                {t('common.cancel')}
              </Button>
              <Button
                disabled={
                  validateSpec(specOf(domainEdit.cache)).length > 0 ||
                  (domainEdit.free && !isDomainOf(aliases, normalizeCacheHost(domainEdit.host)))
                }
                onClick={() => {
                  const host = normalizeCacheHost(domainEdit.host);
                  const entry = { host, cache: pruneSettings(domainEdit.cache) };
                  const at =
                    domainEdit.index >= 0
                      ? domainEdit.index
                      : domains.findIndex((d) => d.host === host);
                  if (isEmptySettings(entry.cache)) {
                    setDomains(domains.filter((_, j) => j !== at));
                  } else if (at >= 0) {
                    setDomains(domains.map((d, j) => (j === at ? entry : d)));
                  } else {
                    setDomains([...domains, entry]);
                  }
                  setDomainEdit(null);
                }}
              >
                {t('cache.tab.applyOverride')}
              </Button>
            </Group>
          </Stack>
        </Modal>
      ) : null}

      {behaviorEdit !== null ? (
        <Modal
          opened
          size="xl"
          onClose={() => {
            setBehaviorEdit(null);
          }}
          title={
            behaviorEdit.index >= 0
              ? t('cache.tab.editBehavior', { n: behaviorEdit.index + 1 })
              : t('cache.tab.addBehavior')
          }
        >
          <Stack gap="sm">
            <PatternField
              label={t('cache.tab.pathPattern')}
              value={behaviorEdit.pathPattern}
              placeholders={{ glob: '/assets/**', prefix: '/assets/', regex: '^/api/' }}
              onChange={(pathPattern) => {
                setBehaviorEdit({ ...behaviorEdit, pathPattern });
              }}
            />
            <SettingsFields
              level="behavior"
              settings={behaviorEdit.cache}
              policies={policyNames}
              onChange={(c) => {
                setBehaviorEdit({ ...behaviorEdit, cache: c });
              }}
            />
            <Group justify="flex-end" gap="xs">
              <Button
                variant="default"
                onClick={() => {
                  setBehaviorEdit(null);
                }}
              >
                {t('common.cancel')}
              </Button>
              <Button
                disabled={
                  behaviorEdit.pathPattern === undefined ||
                  behaviorEdit.pathPattern.pattern.trim() === '' ||
                  patternIssue(behaviorEdit.pathPattern) !== undefined ||
                  validateSpec(specOf(behaviorEdit.cache)).length > 0
                }
                onClick={() => {
                  if (behaviorEdit.pathPattern === undefined) return;
                  const entry = {
                    pathPattern: behaviorEdit.pathPattern,
                    cache: pruneSettings(behaviorEdit.cache),
                  };
                  setBehaviors(
                    behaviorEdit.index >= 0
                      ? behaviors.map((b, j) => (j === behaviorEdit.index ? entry : b))
                      : [...behaviors, entry],
                  );
                  setBehaviorEdit(null);
                }}
              >
                {t('cache.tab.applyBehavior')}
              </Button>
            </Group>
          </Stack>
        </Modal>
      ) : null}
      <Text size="xs" c="dimmed">
        {t('cache.tab.updatedAt', {
          date:
            base?.updatedAt !== undefined
              ? new Date(base.updatedAt).toLocaleString(locale)
              : t('cache.tab.never'),
        })}
      </Text>
    </Stack>
  );
}
