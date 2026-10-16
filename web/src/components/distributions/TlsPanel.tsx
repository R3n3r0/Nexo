import { Alert, Anchor, Badge, Group, Paper, Select, Stack, Text, Title } from '@mantine/core';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { ChoiceField } from '@/components/common/ChoiceField';
import { distributionKeys } from '@/lib/accessQueries';
import { tlsMode, wildcardNeedingDns } from '@/lib/distributions';
import { problemMessage } from '@/lib/problem';
import { useServices, type AcmeChallenge, type Distribution } from '@/services';

import { CertificateStatusBadge } from './CertificateStatusBadge';

export interface TlsPanelProps {
  draft: Distribution;
  onChange: (next: Distribution) => void;
  readOnly: boolean;
  /** The caller may read the certificates (admin:ManageCertificates). */
  canReadCertificates: boolean;
}

/**
 * D-DST-07: the TLS of the domains of a distribution: terminated by a
 * proxy, ACME (the leader obtains and renews the certificate, served by
 * SNI on every node) or an uploaded certificate, with its expiry.
 */
export function TlsPanel({ draft, onChange, readOnly, canReadCertificates }: TlsPanelProps) {
  const { t } = useTranslation();
  const { certificates } = useServices();
  const list = useQuery({
    queryKey: distributionKeys.certificates,
    queryFn: () => certificates.list(),
    enabled: canReadCertificates,
  });
  const mode = tlsMode(draft);
  const uploaded = list.data?.items ?? [];
  const current = mode.mode === 'upload' ? uploaded.find((c) => c.id === mode.id) : undefined;
  const managed = list.data?.acme.managed.filter((m) => m.distributions.includes(draft.id)) ?? [];
  const providers = useQuery({
    queryKey: distributionKeys.dnsProviders,
    queryFn: () => certificates.listDnsProviders(),
    enabled: canReadCertificates && mode.mode === 'auto',
  });
  const challenge = draft.tls?.challenge;
  const wildcard = wildcardNeedingDns(draft);

  const set = (certificate: string) => {
    onChange({ ...draft, tls: { certificate } });
  };
  const setChallenge = (value: AcmeChallenge | undefined, dnsProvider?: string) => {
    onChange({
      ...draft,
      tls: {
        certificate: 'auto',
        ...(value !== undefined ? { challenge: value } : {}),
        ...(dnsProvider !== undefined ? { dnsProvider } : {}),
      },
    });
  };

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('distributions.tls.title')}
    >
      <Stack gap="sm">
        <Title order={4}>{t('distributions.tls.title')}</Title>
        <ChoiceField
          label={t('distributions.tls.mode')}
          value={mode.mode}
          disabled={readOnly}
          choices={(['none', 'auto', 'upload'] as const).map((m) => ({
            value: m,
            label: t(`distributions.tls.short.${m}`),
            description: `${t(`distributions.tls.modes.${m}`)}: ${t(`distributions.tls.help.${m}`)}`,
            disabled: m === 'upload' && uploaded.length === 0 && mode.mode !== 'upload',
          }))}
          onChange={(value) => {
            if (value === 'none') set('none');
            else if (value === 'auto') set('auto');
            else set(`upload:${uploaded[0]?.id ?? ''}`);
          }}
        />
        {!canReadCertificates ? (
          <Text size="xs" c="dimmed">
            {t('distributions.tls.noPermission')}
          </Text>
        ) : list.isError ? (
          <Alert color="red" variant="light">
            {problemMessage(t, list.error)}
          </Alert>
        ) : null}
        {mode.mode === 'upload' ? (
          <Group align="flex-end" gap="sm" wrap="wrap">
            <Select
              label={t('distributions.tls.certificate')}
              data={uploaded.map((c) => ({
                value: c.id,
                label: `${c.id} (${c.domains.join(', ')})`,
              }))}
              value={mode.id ?? null}
              disabled={readOnly}
              allowDeselect={false}
              miw={280}
              onChange={(value) => {
                if (value !== null) set(`upload:${value}`);
              }}
            />
            {current !== undefined ? <CertificateStatusBadge certificate={current} /> : null}
          </Group>
        ) : null}
        {mode.mode === 'auto' ? (
          <Group align="flex-start" gap="sm" wrap="wrap">
            <Select
              label={t('distributions.tls.challenge')}
              description={t('distributions.tls.challengeHint')}
              data={[
                {
                  value: '',
                  label: t('distributions.tls.challengeDefault', {
                    challenge: list.data?.acme.challenge ?? 'http-01',
                  }),
                },
                ...(['http-01', 'tls-alpn-01', 'dns-01'] as const).map((c) => ({
                  value: c,
                  label: t(`distributions.tls.challenges.${c}`),
                })),
              ]}
              value={challenge ?? ''}
              disabled={readOnly}
              allowDeselect={false}
              miw={280}
              onChange={(value) => {
                if (value === null) return;
                if (value === 'dns-01') {
                  setChallenge('dns-01', draft.tls?.dnsProvider ?? providers.data?.[0]?.name ?? '');
                } else setChallenge(value === '' ? undefined : (value as AcmeChallenge));
              }}
            />
            {challenge === 'dns-01' ? (
              <Select
                label={t('distributions.tls.dnsProvider')}
                placeholder={t('distributions.tls.dnsProviderPlaceholder')}
                description={
                  providers.data !== undefined && providers.data.length === 0
                    ? t('distributions.tls.noDnsProvider')
                    : undefined
                }
                data={(providers.data ?? []).map((p) => ({
                  value: p.name,
                  label: `${p.name} (${t(`certificates.dns.types.${p.type}`)})`,
                }))}
                value={draft.tls?.dnsProvider === '' ? null : (draft.tls?.dnsProvider ?? null)}
                disabled={readOnly}
                allowDeselect={false}
                miw={240}
                onChange={(value) => {
                  if (value !== null) setChallenge('dns-01', value);
                }}
              />
            ) : null}
          </Group>
        ) : null}
        {wildcard !== undefined ? (
          <Alert color="orange" variant="light" role="alert">
            {t('distributions.tls.wildcardNeedsDns', { alias: wildcard })}
          </Alert>
        ) : null}
        {mode.mode === 'auto' && list.data !== undefined ? (
          <Stack gap={4}>
            <Text size="sm">
              {t('distributions.tls.acme', {
                directory: list.data.acme.directory,
                challenge: list.data.acme.challenge,
              })}
            </Text>
            {managed.map((m) => (
              <Group key={m.id} gap="xs">
                <Badge color={m.state === 'issued' ? 'teal' : 'yellow'} variant="light">
                  {t(`certificates.managedStates.${m.state}`)}
                </Badge>
                <Text size="sm">{m.domains.join(', ')}</Text>
                {m.certificate !== undefined ? (
                  <CertificateStatusBadge certificate={m.certificate} />
                ) : null}
              </Group>
            ))}
          </Stack>
        ) : null}
        <Anchor component={Link} to="/distributions/-/certificates" size="sm">
          {t('distributions.tls.manage')}
        </Anchor>
      </Stack>
    </Paper>
  );
}
