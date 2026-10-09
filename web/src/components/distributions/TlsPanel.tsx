import { Alert, Anchor, Badge, Group, Paper, Select, Stack, Text, Title } from '@mantine/core';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { ChoiceField } from '@/components/common/ChoiceField';
import { distributionKeys } from '@/lib/accessQueries';
import { tlsMode } from '@/lib/distributions';
import { problemMessage } from '@/lib/problem';
import { useServices, type Distribution } from '@/services';

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

  const set = (certificate: string) => {
    onChange({ ...draft, tls: { certificate } });
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
