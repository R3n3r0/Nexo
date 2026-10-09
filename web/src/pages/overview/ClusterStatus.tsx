import { Anchor, Badge, Group, List, Paper, Stack, Text, Title } from '@mantine/core';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { clusterHealth, formatSkew, setStateColor } from '@/lib/cluster';
import { formatNumber } from '@/lib/format';
import type { ServerInfo } from '@/services';

/**
 * The health of the deployment on the overview: the worst erasure set
 * state, the nodes and drives that do not answer or heal, the background
 * healing and the clock warnings, with a link to the cluster page.
 */
export function ClusterStatus({ info }: { info: ServerInfo }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const health = clusterHealth(info);
  const title = t('overview.cluster.title');
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Title order={4}>{title}</Title>
          <Badge color={setStateColor[health.state]} variant="light" size="lg">
            {t(`cluster.setStates.${health.state}`)}
          </Badge>
        </Group>
        <Text size="sm">
          {t('overview.cluster.summary', {
            nodes: t('overview.ofTotal', { online: health.nodesOnline, total: health.nodes }),
            drives: t('overview.ofTotal', { online: health.drivesOnline, total: health.drives }),
            sets: t('overview.ofTotal', { online: health.setsOk, total: health.sets }),
          })}
        </Text>
        {health.offlineNodes.length > 0 ? (
          <Stack gap={2}>
            <Text size="sm" c="red" fw={600}>
              {t('cluster.alerts.nodesOffline', { count: health.offlineNodes.length })}
            </Text>
            <List size="sm">
              {health.offlineNodes.map((n) => (
                <List.Item key={n}>{n}</List.Item>
              ))}
            </List>
          </Stack>
        ) : null}
        {health.drivesHealing > 0 || health.healPending > 0 ? (
          <Text size="sm" c="yellow.8">
            {t('overview.cluster.healing', {
              drives: formatNumber(health.drivesHealing, locale),
              pending: formatNumber(health.healPending, locale),
            })}
          </Text>
        ) : null}
        {health.skewedNodes.length > 0 && health.maxSkewMs !== undefined ? (
          <Text size="sm" c="yellow.8">
            {t('overview.cluster.clockSkew', {
              skew: formatSkew(health.maxSkewMs, locale).replace('+', ''),
              nodes: health.skewedNodes.join(', '),
            })}
          </Text>
        ) : null}
        <Anchor component={Link} to="/cluster" size="sm">
          {t('overview.cluster.open')}
        </Anchor>
      </Stack>
    </Paper>
  );
}
