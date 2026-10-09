import { Badge, Tooltip } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { daysLeft, expiryState } from '@/lib/distributions';
import type { CertificateDetails } from '@/services';

/**
 * The validity of a certificate: valid, expiring (fewer than 21 days left,
 * highlighted), expired; the days left in the label.
 */
export function CertificateStatusBadge({
  certificate,
}: {
  certificate: Pick<CertificateDetails, 'notAfter' | 'status'>;
}) {
  const { t, i18n } = useTranslation();
  const state = expiryState(certificate);
  const days = daysLeft(certificate.notAfter);
  // Yellow with dark text: the white text of a filled orange badge is
  // under the 4.5:1 contrast.
  const color = state === 'expired' ? 'red' : state === 'expiring' ? 'yellow' : 'teal';
  return (
    <Tooltip label={new Date(certificate.notAfter).toLocaleString(i18n.resolvedLanguage)}>
      <Badge
        color={color}
        variant={state === 'ok' ? 'light' : 'filled'}
        autoContrast
        data-expiry={state}
      >
        {state === 'expired'
          ? t('certificates.expired')
          : state === 'expiring'
            ? t('certificates.expiringIn', { count: days })
            : t('certificates.validFor', { count: days })}
      </Badge>
    </Tooltip>
  );
}
