import { Anchor, Badge, Button, Group, Paper, Skeleton, Stack, Table, Text } from '@mantine/core';
import { IconPlus } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { DistributionActions, usePermissions } from '@/auth/usePermissions';
import { CertificateStatusBadge } from '@/components/distributions/CertificateStatusBadge';
import { ErrorAlert } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { distributionKeys } from '@/lib/accessQueries';
import { tlsMode, trafficTotals } from '@/lib/distributions';
import { formatBytes, formatNumber } from '@/lib/format';
import {
  isApiError,
  useServices,
  type Certificate,
  type Distribution,
  type ManagedCertificate,
} from '@/services';

import { DistributionsHeader } from './DistributionsHeader';

function TlsCell({
  d,
  certificates,
  managed,
}: {
  d: Distribution;
  certificates: readonly Certificate[] | undefined;
  managed: readonly ManagedCertificate[] | undefined;
}) {
  const { t } = useTranslation();
  const mode = tlsMode(d);
  if (mode.mode === 'none') {
    return (
      <Text size="xs" c="dimmed">
        {t('distributions.tls.modes.none')}
      </Text>
    );
  }
  if (mode.mode === 'upload') {
    const cert = certificates?.find((c) => c.id === mode.id);
    return (
      <Group gap={4} wrap="nowrap">
        <Text size="xs" ff="monospace">
          {mode.id}
        </Text>
        {cert !== undefined ? <CertificateStatusBadge certificate={cert} /> : null}
      </Group>
    );
  }
  const m = managed?.find((x) => x.distributions.includes(d.id));
  return (
    <Group gap={4} wrap="nowrap">
      <Badge size="sm" variant="outline" color="gray">
        ACME
      </Badge>
      {m?.certificate !== undefined ? (
        <CertificateStatusBadge certificate={m.certificate} />
      ) : m !== undefined ? (
        <Badge size="sm" color="yellow" variant="light">
          {t(`certificates.managedStates.${m.state}`)}
        </Badge>
      ) : null}
    </Group>
  );
}

/**
 * D-DST-01: the distributions with their domains, origin, mode, state,
 * certificates (expiry highlighted) and traffic of the aliases.
 */
export function DistributionsPage() {
  const { t, i18n } = useTranslation();
  const { distributions, certificates } = useServices();
  const perms = usePermissions();
  const canManage = perms.can(DistributionActions.manage);
  const canCerts = perms.can(DistributionActions.certificates);
  const list = useQuery({ queryKey: distributionKeys.list, queryFn: () => distributions.list() });
  const certs = useQuery({
    queryKey: distributionKeys.certificates,
    queryFn: () => certificates.list(),
    enabled: canCerts,
  });
  const traffic = useQuery({
    queryKey: distributionKeys.traffic(),
    queryFn: () => distributions.traffic(),
    enabled: list.isSuccess,
    retry: false,
  });
  const locale = i18n.resolvedLanguage;
  const forbidden = list.isError && isApiError(list.error) && list.error.isForbidden;

  return (
    <Stack gap="md">
      <DistributionsHeader
        active="list"
        actions={
          <PermissionGuard allowed={canManage} action={DistributionActions.manage}>
            {(disabled) => (
              <Button
                component={Link}
                to="/distributions/-/new"
                leftSection={<IconPlus size={16} />}
                disabled={disabled}
                onClick={(event) => {
                  if (disabled) event.preventDefault();
                }}
              >
                {t('distributions.create')}
              </Button>
            )}
          </PermissionGuard>
        }
      />
      {forbidden ? (
        <ErrorAlert error={list.error} title={t('distributions.forbidden')} />
      ) : list.isError ? (
        <ErrorAlert
          error={list.error}
          title={t('distributions.loadError')}
          onRetry={() => {
            void list.refetch();
          }}
        />
      ) : list.isPending ? (
        <Skeleton height={160} />
      ) : list.data.length === 0 ? (
        <Paper withBorder p="xl" radius="md">
          <Stack gap={4} align="center">
            <Text fw={500}>{t('distributions.empty')}</Text>
            <Text size="sm" c="dimmed">
              {t('distributions.emptyHint')}
            </Text>
          </Stack>
        </Paper>
      ) : (
        <Paper withBorder radius="md">
          <Table.ScrollContainer minWidth={900}>
            <Table aria-label={t('distributions.tableLabel')} verticalSpacing="sm" highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('distributions.fields.id')}</Table.Th>
                  <Table.Th>{t('distributions.fields.aliases')}</Table.Th>
                  <Table.Th>{t('distributions.fields.origin')}</Table.Th>
                  <Table.Th>{t('distributions.fields.mode')}</Table.Th>
                  <Table.Th>{t('common.status')}</Table.Th>
                  <Table.Th>{t('distributions.fields.tls')}</Table.Th>
                  <Table.Th ta="right">{t('distributions.traffic.title')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {list.data.map((d) => {
                  const hosts = traffic.data?.hosts.filter((h) => h.distribution === d.id) ?? [];
                  const totals = trafficTotals(hosts);
                  return (
                    <Table.Tr key={d.id}>
                      <Table.Td>
                        <Anchor
                          component={Link}
                          to={`/distributions/${encodeURIComponent(d.id)}`}
                          fw={500}
                        >
                          {d.id}
                        </Anchor>
                        {d.comment !== undefined && d.comment !== '' ? (
                          <Text size="xs" c="dimmed">
                            {d.comment}
                          </Text>
                        ) : null}
                      </Table.Td>
                      <Table.Td>
                        <Group gap={4}>
                          {d.aliases.map((a) => (
                            <Badge key={a} variant="outline" color="gray" tt="none">
                              {a}
                            </Badge>
                          ))}
                        </Group>
                      </Table.Td>
                      <Table.Td>
                        <Text size="sm" ff="monospace">
                          {d.origin.bucket}
                          {d.origin.pathPrefix ?? ''}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Badge variant="light">
                          {t(`distributions.modes.${d.mode ?? 'website'}`)}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <Badge color={d.enabled === false ? 'gray' : 'teal'} variant="light">
                          {d.enabled === false
                            ? t('distributions.disabled')
                            : t('distributions.enabled')}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <TlsCell
                          d={d}
                          certificates={certs.data?.items}
                          managed={certs.data?.acme.managed}
                        />
                      </Table.Td>
                      <Table.Td ta="right">
                        {traffic.data === null || traffic.isError ? (
                          <Text size="xs" c="dimmed">
                            —
                          </Text>
                        ) : traffic.data === undefined ? null : (
                          <Stack gap={0} align="flex-end">
                            <Text size="sm">
                              {t('distributions.traffic.requestsCount', {
                                count: totals.requests,
                                formatted: formatNumber(totals.requests, locale),
                              })}
                            </Text>
                            <Text size="xs" c="dimmed">
                              {formatBytes(totals.bytes, locale)}
                            </Text>
                          </Stack>
                        )}
                      </Table.Td>
                    </Table.Tr>
                  );
                })}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Paper>
      )}
    </Stack>
  );
}
