import {
  Alert,
  Anchor,
  Button,
  Code,
  Group,
  Paper,
  Select,
  SimpleGrid,
  Stack,
  Switch,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconCheck } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router';

import { DistributionActions, usePermissions } from '@/auth/usePermissions';
import { AliasesField } from '@/components/distributions/AliasesField';
import { DnsInstructions } from '@/components/distributions/DnsInstructions';
import { ChoiceField } from '@/components/common/ChoiceField';
import { ErrorAlert } from '@/components/iam/common';
import { distributionKeys } from '@/lib/accessQueries';
import {
  accessModes,
  aliasIssue,
  isValidDistributionId,
  newDistribution,
  pathPrefixIssue,
  tlsMode,
} from '@/lib/distributions';
import { storageKeys } from '@/lib/storageKeys';
import { toYaml } from '@/lib/yaml';
import { useServices, type Distribution, type DistributionAccess } from '@/services';

import { DistributionsHeader } from './DistributionsHeader';

const steps = ['origin', 'domains', 'mode', 'access', 'tls', 'review'] as const;

/** The steps of the wizard: done, current, to come; the done ones can be revisited. */
function WizardSteps({
  active,
  labels,
  onSelect,
}: {
  active: number;
  labels: readonly string[];
  onSelect: (step: number) => void;
}) {
  const { t } = useTranslation();
  return (
    <Group
      component="ol"
      gap="xs"
      aria-label={t('wizard.stepsLabel')}
      style={{ listStyle: 'none', padding: 0, margin: 0 }}
    >
      {labels.map((label, i) => {
        const done = i < active;
        const current = i === active;
        return (
          <li key={label}>
            <Button
              size="compact-sm"
              variant={current ? 'filled' : done ? 'light' : 'subtle'}
              color={current || done ? undefined : 'gray'}
              leftSection={
                done ? (
                  <IconCheck size={14} />
                ) : (
                  <Text span size="xs">
                    {i + 1}
                  </Text>
                )
              }
              aria-current={current ? 'step' : undefined}
              disabled={!done || active >= labels.length}
              onClick={() => {
                onSelect(i);
              }}
            >
              {label}
            </Button>
          </li>
        );
      })}
    </Group>
  );
}

/**
 * D-DST-02: the creation wizard of a distribution: bucket and prefix,
 * domains (checked against the routing table), mode, access of the
 * default behavior, TLS; then the DNS records to create and the
 * verification that the domains reach the distribution.
 */
export function DistributionWizard() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { distributions, buckets, keyGroups, certificates } = useServices();
  const perms = usePermissions();
  const [active, setActive] = useState(0);
  const [draft, setDraft] = useState<Distribution>(newDistribution);
  const [touched, setTouched] = useState(false);
  const [aliasConflict, setAliasConflict] = useState(false);
  const bucketList = useQuery({ queryKey: storageKeys.buckets, queryFn: () => buckets.list() });
  const groups = useQuery({
    queryKey: distributionKeys.keyGroups,
    queryFn: () => keyGroups.list(),
  });
  const certs = useQuery({
    queryKey: distributionKeys.certificates,
    queryFn: () => certificates.list(),
    enabled: perms.can(DistributionActions.certificates),
  });
  const create = useMutation({
    mutationFn: (d: Distribution) => distributions.create(d),
    onSuccess: async (stored) => {
      setDraft(stored);
      await queryClient.invalidateQueries({ queryKey: distributionKeys.all });
      setActive(steps.length);
    },
  });

  const behavior = draft.defaultBehavior ?? {};
  const website = (draft.mode ?? 'website') === 'website';
  const access: DistributionAccess = behavior.access ?? 'public';
  const tls = tlsMode(draft);

  const issues = {
    id: !isValidDistributionId(draft.id),
    bucket: draft.origin.bucket === '',
    prefix: pathPrefixIssue(draft.origin.pathPrefix ?? ''),
    aliases:
      draft.aliases.length === 0 ||
      draft.aliases.some((a) => aliasIssue(a) !== undefined) ||
      aliasConflict,
    keyGroup: access === 'signed' && (behavior.signing?.keyGroup ?? '') === '',
    certificate: tls.mode === 'upload' && (tls.id ?? '') === '',
  };
  const stepValid = [
    !issues.id && !issues.bucket && !issues.prefix,
    !issues.aliases,
    true,
    !issues.keyGroup,
    !issues.certificate,
    true,
  ];

  const setBehavior = (patch: Partial<NonNullable<Distribution['defaultBehavior']>>) => {
    setDraft((d) => ({ ...d, defaultBehavior: { ...(d.defaultBehavior ?? {}), ...patch } }));
  };

  const next = () => {
    setTouched(true);
    if (stepValid[active] === true) {
      setTouched(false);
      setActive((a) => Math.min(a + 1, steps.length - 1));
    }
  };

  /** The document sent: the defaults the wizard does not ask for are left out. */
  const document = (): Distribution => {
    const { pathPrefix, ...origin } = draft.origin;
    const prefix = website ? (pathPrefix ?? '').trim() : '';
    const def = { ...(draft.defaultBehavior ?? {}) };
    if (!website || (def.indexDocument ?? '') === '') delete def.indexDocument;
    if (!website || (def.spaFallback ?? '') === '') delete def.spaFallback;
    if (access !== 'signed') delete def.signing;
    const { comment: _c, ...rest } = draft;
    const comment = (draft.comment ?? '').trim();
    return {
      ...rest,
      ...(comment !== '' ? { comment } : {}),
      origin: prefix === '' ? origin : { ...origin, pathPrefix: prefix },
      defaultBehavior: def,
    };
  };

  if (!perms.loading && perms.ready && !perms.can(DistributionActions.manage)) {
    return (
      <Stack gap="md">
        <DistributionsHeader active="list" />
        <Alert color="red" variant="light" role="alert">
          {t('permissions.missing', { action: DistributionActions.manage })}
        </Alert>
      </Stack>
    );
  }

  return (
    <Stack gap="md">
      <DistributionsHeader active="list" />
      <Paper withBorder radius="md" p="md" component="section" aria-label={t('wizard.title')}>
        <Stack gap="md">
          <Title order={3}>{t('wizard.title')}</Title>
          <WizardSteps
            active={active}
            labels={steps.map((step) => t(`wizard.steps.${step}`))}
            onSelect={(step) => {
              if (step < active && active < steps.length) setActive(step);
            }}
          />
          {active === 0 ? (
            <>
              <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm" mt="md">
                <TextInput
                  label={t('distributions.fields.id')}
                  description={t('wizard.idHint')}
                  required
                  value={draft.id}
                  error={touched && issues.id ? t('wizard.issues.id') : undefined}
                  onChange={(event) => {
                    const id = event.currentTarget.value;
                    setDraft((d) => ({ ...d, id }));
                  }}
                />
                <Select
                  label={t('distributions.fields.bucket')}
                  required
                  searchable
                  data={(bucketList.data ?? []).map((b) => b.name)}
                  value={draft.origin.bucket === '' ? null : draft.origin.bucket}
                  error={touched && issues.bucket ? t('wizard.issues.bucket') : undefined}
                  nothingFoundMessage={t('wizard.noBucket')}
                  onChange={(bucket) => {
                    setDraft((d) => ({ ...d, origin: { ...d.origin, bucket: bucket ?? '' } }));
                  }}
                />
                <TextInput
                  label={t('distributions.fields.pathPrefix')}
                  description={t('wizard.prefixHint')}
                  placeholder="/prod"
                  value={draft.origin.pathPrefix ?? ''}
                  error={issues.prefix ? t('wizard.issues.prefix') : undefined}
                  onChange={(event) => {
                    const pathPrefix = event.currentTarget.value;
                    setDraft((d) => ({ ...d, origin: { ...d.origin, pathPrefix } }));
                  }}
                />
                <TextInput
                  label={t('distributions.fields.comment')}
                  value={draft.comment ?? ''}
                  onChange={(event) => {
                    const comment = event.currentTarget.value;
                    setDraft((d) => ({ ...d, comment }));
                  }}
                />
              </SimpleGrid>
            </>
          ) : null}
          {active === 1 ? (
            <>
              <Stack gap="md" mt="md">
                <AliasesField
                  value={draft.aliases}
                  onConflict={setAliasConflict}
                  error={
                    touched && issues.aliases
                      ? aliasConflict
                        ? t('wizard.issues.aliasTaken')
                        : t('wizard.issues.aliases')
                      : undefined
                  }
                  onChange={(aliases) => {
                    setDraft((d) => ({ ...d, aliases }));
                  }}
                />
                <Title order={5}>{t('dns.title')}</Title>
                <DnsInstructions aliases={draft.aliases} />
              </Stack>
            </>
          ) : null}
          {active === 2 ? (
            <>
              <Stack gap="md" mt="md">
                <ChoiceField
                  label={t('distributions.fields.mode')}
                  value={draft.mode ?? 'website'}
                  choices={[
                    {
                      value: 'website',
                      label: t('distributions.modes.website'),
                      description: t('wizard.websiteHint'),
                    },
                    {
                      value: 's3',
                      label: t('distributions.modes.s3'),
                      description: t('wizard.s3Hint'),
                    },
                  ]}
                  onChange={(mode) => {
                    setDraft((d) => ({ ...d, mode }));
                  }}
                />
                {website ? (
                  <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                    <TextInput
                      label={t('distributions.fields.indexDocument')}
                      value={behavior.indexDocument ?? ''}
                      onChange={(event) => {
                        setBehavior({ indexDocument: event.currentTarget.value });
                      }}
                    />
                    <Switch
                      mt={28}
                      label={t('distributions.responses.spa')}
                      checked={(behavior.spaFallback ?? '') !== ''}
                      onChange={(event) => {
                        setBehavior({
                          spaFallback: event.currentTarget.checked ? '/index.html' : '',
                        });
                      }}
                    />
                  </SimpleGrid>
                ) : (
                  <Alert color="blue" variant="light">
                    {t('wizard.s3Note')}
                  </Alert>
                )}
              </Stack>
            </>
          ) : null}
          {active === 3 ? (
            <>
              <Stack gap="md" mt="md">
                <ChoiceField
                  label={t('distributions.fields.access')}
                  value={access}
                  choices={accessModes.map((a) => ({
                    value: a,
                    label: t(`distributions.access.${a}`),
                    description: t(`distributions.accessHelp.${a}`),
                  }))}
                  onChange={(value) => {
                    setBehavior({ access: value });
                  }}
                />
                {access === 'signed' ? (
                  <Group align="flex-end" gap="sm">
                    <Select
                      label={t('distributions.fields.keyGroup')}
                      data={(groups.data ?? []).map((g) => g.id)}
                      value={behavior.signing?.keyGroup ?? null}
                      error={
                        touched && issues.keyGroup
                          ? t('distributions.behaviors.keyGroupRequired')
                          : undefined
                      }
                      onChange={(keyGroup) => {
                        setBehavior({ signing: { keyGroup: keyGroup ?? '' } });
                      }}
                      miw={240}
                    />
                    <Anchor component={Link} to="/distributions/-/key-groups" size="sm" pb={8}>
                      {t('wizard.manageKeyGroups')}
                    </Anchor>
                  </Group>
                ) : null}
              </Stack>
            </>
          ) : null}
          {active === 4 ? (
            <>
              <Stack gap="md" mt="md">
                <ChoiceField
                  label={t('distributions.tls.mode')}
                  value={tls.mode}
                  choices={(['none', 'auto', 'upload'] as const).map((m) => ({
                    value: m,
                    label: t(`distributions.tls.short.${m}`),
                    description: `${t(`distributions.tls.modes.${m}`)}: ${t(`distributions.tls.help.${m}`)}`,
                    disabled: m === 'upload' && (certs.data?.items.length ?? 0) === 0,
                  }))}
                  onChange={(value) => {
                    setDraft((d) => ({
                      ...d,
                      tls: {
                        certificate:
                          value === 'upload' ? `upload:${certs.data?.items[0]?.id ?? ''}` : value,
                      },
                    }));
                  }}
                />
                {tls.mode === 'upload' ? (
                  <Select
                    label={t('distributions.tls.certificate')}
                    data={(certs.data?.items ?? []).map((c) => ({
                      value: c.id,
                      label: `${c.id} (${c.domains.join(', ')})`,
                    }))}
                    value={tls.id === '' ? null : (tls.id ?? null)}
                    error={
                      touched && issues.certificate ? t('wizard.issues.certificate') : undefined
                    }
                    onChange={(id) => {
                      setDraft((d) => ({ ...d, tls: { certificate: `upload:${id ?? ''}` } }));
                    }}
                    maw={420}
                  />
                ) : null}
              </Stack>
            </>
          ) : null}
          {active === 5 ? (
            <>
              <Stack gap="sm" mt="md">
                <Text size="sm">{t('wizard.reviewHint')}</Text>
                <Code block aria-label={t('wizard.document')}>
                  {toYaml(document())}
                </Code>
                {create.isError ? (
                  <ErrorAlert error={create.error} title={t('wizard.createFailed')} />
                ) : null}
              </Stack>
            </>
          ) : null}
          {active === steps.length ? (
            <>
              <Stack gap="md" mt="md">
                <Alert
                  color="teal"
                  variant="light"
                  icon={<IconCheck size={18} />}
                  title={t('wizard.created', { id: draft.id })}
                >
                  {t('wizard.createdHint')}
                </Alert>
                <Title order={5}>{t('dns.title')}</Title>
                <DnsInstructions aliases={draft.aliases} distribution={draft.id} />
                <Group>
                  <Button
                    onClick={() => {
                      void navigate(`/distributions/${encodeURIComponent(draft.id)}`);
                    }}
                  >
                    {t('wizard.open')}
                  </Button>
                  <Button component={Link} to="/distributions" variant="default">
                    {t('wizard.backToList')}
                  </Button>
                </Group>
              </Stack>
            </>
          ) : null}
          {active < steps.length ? (
            <Group justify="space-between">
              <Button
                variant="default"
                disabled={active === 0}
                onClick={() => {
                  setActive((a) => Math.max(0, a - 1));
                }}
              >
                {t('wizard.back')}
              </Button>
              {active < steps.length - 1 ? (
                <Button onClick={next}>{t('wizard.next')}</Button>
              ) : (
                <Button
                  loading={create.isPending}
                  onClick={() => {
                    create.mutate(document());
                  }}
                >
                  {t('wizard.create')}
                </Button>
              )}
            </Group>
          ) : null}
        </Stack>
      </Paper>
    </Stack>
  );
}
