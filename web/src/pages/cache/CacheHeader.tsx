import { Button, Group } from '@mantine/core';
import {
  IconAdjustments,
  IconChartBar,
  IconEraser,
  IconFlame,
  IconListDetails,
  IconSearch,
} from '@tabler/icons-react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { PageHeader } from '@/components/iam/common';

import type { CacheTab } from './cacheTabs';

const icons: Record<CacheTab, ReactNode> = {
  overview: <IconChartBar size={16} />,
  policies: <IconListDetails size={16} />,
  invalidations: <IconEraser size={16} />,
  warm: <IconFlame size={16} />,
  inspector: <IconSearch size={16} />,
  settings: <IconAdjustments size={16} />,
};

/** Title and sections of the cache pages (D-CCH-01..07). */
export function CacheHeader({
  active,
  tabs,
  actions,
}: {
  active: CacheTab;
  tabs: readonly CacheTab[];
  actions?: ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <>
      <PageHeader title={t('nav.cache')} description={t('cache.subtitle')} actions={actions} />
      <Group gap={4} component="nav" aria-label={t('cache.sections')} wrap="wrap">
        {tabs.map((tab) => (
          <Button
            key={tab}
            component={Link}
            to={`/cache/${tab}`}
            variant={active === tab ? 'light' : 'subtle'}
            color={active === tab ? undefined : 'gray'}
            leftSection={icons[tab]}
            aria-current={active === tab ? 'page' : undefined}
          >
            {t(`cache.tabs.${tab}`)}
          </Button>
        ))}
      </Group>
    </>
  );
}
