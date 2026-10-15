import { Badge, Group, Stack, Table, Text } from '@mantine/core';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { ScrollTable } from '@/components/cache/ScrollTable';
import { certificateState } from '@/lib/monitoring';
import type { TelemetryInfo, TlsInfo } from '@/services';

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Table.Tr>
      <Table.Th w={220}>{label}</Table.Th>
      <Table.Td style={{ overflowWrap: 'anywhere' }}>{children}</Table.Td>
    </Table.Tr>
  );
}

const stateColors = {
  valid: 'teal',
  expiring: 'yellow',
  expired: 'red',
  notYetValid: 'orange',
} as const;

/**
 * The TLS of the listeners of the node that answers (docs/07 §7.3): on or
 * off, the minimum version, the mutual TLS between the nodes and the
 * certificates of the certificates directory with their expiry (21 days
 * before it they are flagged, like the warning of the server logs).
 */
export function TlsStatus({ tls, now = new Date() }: { tls: TlsInfo | undefined; now?: Date }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  if (tls === undefined) {
    return (
      <Text size="sm" c="dimmed">
        {t('observability.tls.notReported')}
      </Text>
    );
  }
  return (
    <Stack gap="sm">
      <Table variant="vertical" layout="fixed" aria-label={t('observability.tls.title')}>
        <Table.Tbody>
          <Row label={t('observability.tls.state')}>
            <Badge color={tls.enabled ? 'teal' : 'gray'} variant="light">
              {tls.enabled ? t('observability.tls.enabled') : t('observability.tls.disabled')}
            </Badge>
          </Row>
          {tls.enabled ? (
            <>
              <Row label={t('observability.tls.minVersion')}>TLS {tls.minVersion ?? '1.2'}</Row>
              <Row label={t('observability.tls.internode')}>
                {tls.internodeVerify ? t('observability.tls.yes') : t('observability.tls.no')}
              </Row>
            </>
          ) : null}
        </Table.Tbody>
      </Table>
      {tls.enabled ? (
        tls.certificates.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('observability.tls.none')}
          </Text>
        ) : (
          <ScrollTable minWidth={720} label={t('observability.tls.certificates')}>
            <Table aria-label={t('observability.tls.certificates')} verticalSpacing="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('observability.tls.certificate')}</Table.Th>
                  <Table.Th>{t('observability.tls.names')}</Table.Th>
                  <Table.Th>{t('observability.tls.issuer')}</Table.Th>
                  <Table.Th>{t('observability.tls.expires')}</Table.Th>
                  <Table.Th>{t('observability.tls.status')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {tls.certificates.map((c) => {
                  const state = certificateState(c, now);
                  return (
                    <Table.Tr key={`${c.name}-${c.fingerprint}`}>
                      <Table.Td>
                        <Stack gap={0}>
                          <Text size="sm">
                            {c.name === '' ? t('observability.tls.default') : c.name}
                          </Text>
                          <Text
                            size="xs"
                            c="dimmed"
                            ff="monospace"
                            title={`${t('observability.tls.fingerprint')}: ${c.fingerprint}`}
                          >
                            {c.fingerprint.slice(0, 16)}…
                          </Text>
                        </Stack>
                      </Table.Td>
                      <Table.Td style={{ overflowWrap: 'anywhere' }}>
                        {[...(c.domains ?? []), ...(c.ips ?? [])].join(', ') || c.subject}
                      </Table.Td>
                      <Table.Td style={{ overflowWrap: 'anywhere' }}>{c.issuer}</Table.Td>
                      <Table.Td>
                        <time dateTime={c.notAfter}>
                          {new Date(c.notAfter).toLocaleDateString(locale)}
                        </time>
                      </Table.Td>
                      <Table.Td>
                        <Badge color={stateColors[state.state]} variant="light">
                          {state.state === 'expiring'
                            ? t('observability.tls.expiring', { days: state.days })
                            : t(`observability.tls.${state.state}`)}
                        </Badge>
                      </Table.Td>
                    </Table.Tr>
                  );
                })}
              </Table.Tbody>
            </Table>
          </ScrollTable>
        )
      ) : null}
    </Stack>
  );
}

/**
 * The distributed tracing (OpenTelemetry, docs/07 §7.4) of the node that
 * answers: exporting or not, the collector, the protocol, the sampling.
 */
export function TelemetryStatus({ telemetry }: { telemetry: TelemetryInfo | undefined }) {
  const { t, i18n } = useTranslation();
  if (telemetry === undefined) {
    return (
      <Text size="sm" c="dimmed">
        {t('observability.telemetry.notReported')}
      </Text>
    );
  }
  const ratio = new Intl.NumberFormat(i18n.resolvedLanguage, {
    style: 'percent',
    maximumFractionDigits: 1,
  }).format(telemetry.sampleRatio);
  return (
    <Table variant="vertical" layout="fixed" aria-label={t('observability.telemetry.title')}>
      <Table.Tbody>
        <Row label={t('observability.telemetry.state')}>
          <Group gap="xs">
            <Badge color={telemetry.enabled ? 'teal' : 'gray'} variant="light">
              {telemetry.enabled
                ? t('observability.telemetry.enabled')
                : t('observability.telemetry.disabled')}
            </Badge>
          </Group>
        </Row>
        {telemetry.endpoint !== undefined && telemetry.endpoint !== '' ? (
          <>
            <Row label={t('observability.telemetry.endpoint')}>
              <Text size="sm" ff="monospace">
                {telemetry.endpoint}
              </Text>
            </Row>
            <Row label={t('observability.telemetry.protocol')}>{telemetry.protocol ?? '—'}</Row>
          </>
        ) : null}
        <Row label={t('observability.telemetry.sampleRatio')}>{ratio}</Row>
      </Table.Tbody>
    </Table>
  );
}
