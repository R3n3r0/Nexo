import { Anchor, Group, Text } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { licenseUrl, sourceRepository, sourceUrl } from '@/lib/legal';

export interface LegalNoticeProps {
  /** Version of the server, when known. */
  version?: string;
  /** Commit of the server, when known: links the source of that build. */
  commit?: string;
}

/**
 * The Appropriate Legal Notices of the console (AGPL-3.0 §5(d), §13 and the
 * §7(b) attribution of NOTICE): version, license, "Powered by Nexo" and the
 * link to the source code.
 */
export function LegalNotice({ version, commit }: LegalNoticeProps) {
  const { t } = useTranslation();
  const separator = (
    <Text span size="xs" c="dimmed" aria-hidden>
      —
    </Text>
  );
  return (
    <Group
      component="footer"
      gap={4}
      justify="center"
      wrap="wrap"
      aria-label={t('legal.label')}
      data-testid="legal-notice"
    >
      <Text span size="xs" c="dimmed">
        {version !== undefined && version !== '' ? `Nexo ${version}` : 'Nexo'}
      </Text>
      {separator}
      <Anchor href={licenseUrl} target="_blank" rel="noopener noreferrer" size="xs" c="dimmed">
        {t('legal.license')}
      </Anchor>
      {separator}
      <Anchor
        href={sourceRepository}
        target="_blank"
        rel="noopener noreferrer"
        size="xs"
        c="dimmed"
      >
        {t('legal.poweredBy')}
      </Anchor>
      {separator}
      <Anchor
        href={sourceUrl(version, commit)}
        target="_blank"
        rel="noopener noreferrer"
        size="xs"
        c="dimmed"
      >
        {t('legal.source')}
      </Anchor>
    </Group>
  );
}
