import { Button, Group } from '@mantine/core';
import { IconCertificate, IconKey, IconWorld } from '@tabler/icons-react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { DistributionActions, usePermissions } from '@/auth/usePermissions';
import { PageHeader } from '@/components/iam/common';

export type DistributionsSection = 'list' | 'key-groups' | 'certificates';

/**
 * Title and sub-sections of the distributions (D-DST-01, 05, 07): the
 * distributions, the key groups of the signed URLs and the certificates
 * (shown with admin:ManageCertificates).
 */
export function DistributionsHeader({
  active,
  actions,
}: {
  active: DistributionsSection;
  actions?: ReactNode;
}) {
  const { t } = useTranslation();
  const perms = usePermissions();
  const showCertificates = !perms.ready || perms.can(DistributionActions.certificates);
  const links: { key: DistributionsSection; to: string; label: string; icon: ReactNode }[] = [
    {
      key: 'list',
      to: '/distributions',
      label: t('distributions.tabs.list'),
      icon: <IconWorld size={16} />,
    },
    {
      key: 'key-groups',
      to: '/distributions/-/key-groups',
      label: t('distributions.tabs.keyGroups'),
      icon: <IconKey size={16} />,
    },
    ...(showCertificates
      ? [
          {
            key: 'certificates' as const,
            to: '/distributions/-/certificates',
            label: t('distributions.tabs.certificates'),
            icon: <IconCertificate size={16} />,
          },
        ]
      : []),
  ];
  return (
    <>
      <PageHeader
        title={t('nav.distributions')}
        description={t('distributions.subtitle')}
        actions={actions}
      />
      <Group gap={4} component="nav" aria-label={t('distributions.sections')}>
        {links.map((l) => (
          <Button
            key={l.key}
            component={Link}
            to={l.to}
            variant={active === l.key ? 'light' : 'subtle'}
            color={active === l.key ? undefined : 'gray'}
            leftSection={l.icon}
            aria-current={active === l.key ? 'page' : undefined}
          >
            {l.label}
          </Button>
        ))}
      </Group>
    </>
  );
}
