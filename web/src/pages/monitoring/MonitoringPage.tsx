import { Alert, Button, Group, Skeleton, Stack } from '@mantine/core';
import { IconChartLine, IconFileText, IconListSearch, IconTimeline } from '@tabler/icons-react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, Navigate, useParams } from 'react-router';

import { MonitoringActions, usePermissions, type PermissionCheck } from '@/auth/usePermissions';
import { PageHeader } from '@/components/iam/common';

import { AuditTab } from './AuditTab';
import { LogsTab } from './LogsTab';
import { MetricsTab } from './MetricsTab';
import { TraceTab } from './TraceTab';

/** The sections of the monitoring page, in their order. */
const monitoringTabs = ['metrics', 'trace', 'logs', 'audit'] as const;
type MonitoringTab = (typeof monitoringTabs)[number];

const icons: Record<MonitoringTab, ReactNode> = {
  metrics: <IconChartLine size={16} />,
  trace: <IconTimeline size={16} />,
  logs: <IconFileText size={16} />,
  audit: <IconListSearch size={16} />,
};

/**
 * The sections a user may open: the charts with admin:Prometheus or
 * admin:ServerInfo, the trace with admin:ServerTrace, the logs and the
 * audit log with admin:ConsoleLog (the authorization of the Admin API).
 */
function visibleTabs(perms: Pick<PermissionCheck, 'can'>): MonitoringTab[] {
  return monitoringTabs.filter((tab) => {
    switch (tab) {
      case 'metrics':
        return perms.can(MonitoringActions.prometheus) || perms.can(MonitoringActions.serverInfo);
      case 'trace':
        return perms.can(MonitoringActions.trace);
      default:
        return perms.can(MonitoringActions.consoleLog);
    }
  });
}

function Header({ active, tabs }: { active: MonitoringTab | undefined; tabs: MonitoringTab[] }) {
  const { t } = useTranslation();
  return (
    <>
      <PageHeader title={t('nav.monitoring')} description={t('monitoring.subtitle')} />
      <Group gap={4} component="nav" aria-label={t('monitoring.sections')} wrap="wrap">
        {tabs.map((tab) => (
          <Button
            key={tab}
            component={Link}
            to={`/monitoring/${tab}`}
            variant={active === tab ? 'light' : 'subtle'}
            color={active === tab ? undefined : 'gray'}
            leftSection={icons[tab]}
            aria-current={active === tab ? 'page' : undefined}
          >
            {t(`monitoring.tabs.${tab}`)}
          </Button>
        ))}
      </Group>
    </>
  );
}

/**
 * Monitoring (D-MON-01..03): the charts of the metrics, the live trace of
 * the requests, the live logs and the search of the audit log, each a
 * section shown only to the users allowed to read it (D-UX-02).
 */
export function MonitoringPage() {
  const { t } = useTranslation();
  const { tab } = useParams();
  const perms = usePermissions();
  if (!perms.ready) {
    return perms.loading ? (
      <Skeleton height={320} />
    ) : (
      <Alert color="red" variant="light" role="alert">
        {t('monitoring.permissionsError')}
      </Alert>
    );
  }
  const tabs = visibleTabs(perms);
  if (tabs.length === 0) {
    return (
      <Stack gap="md">
        <Header active={undefined} tabs={[]} />
        <Alert color="gray" variant="light" role="alert">
          {t('monitoring.forbidden')}
        </Alert>
      </Stack>
    );
  }
  const first = tabs[0] as MonitoringTab;
  if (tab === undefined) return <Navigate to={`/monitoring/${first}`} replace />;
  const known = (monitoringTabs as readonly string[]).includes(tab);
  const active = tab as MonitoringTab;
  if (!known || !tabs.includes(active)) {
    return (
      <Stack gap="md">
        <Header active={undefined} tabs={tabs} />
        <Alert color="gray" variant="light" role="alert">
          {known
            ? t('monitoring.tabForbidden', { section: t(`monitoring.tabs.${active}`) })
            : t('monitoring.unknownTab')}
        </Alert>
      </Stack>
    );
  }
  return (
    <Stack gap="md">
      <Header active={active} tabs={tabs} />
      {active === 'metrics' ? (
        <MetricsTab />
      ) : active === 'trace' ? (
        <TraceTab />
      ) : active === 'logs' ? (
        <LogsTab />
      ) : (
        <AuditTab />
      )}
    </Stack>
  );
}
