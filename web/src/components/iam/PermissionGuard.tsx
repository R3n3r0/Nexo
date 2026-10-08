import { Tooltip } from '@mantine/core';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

export interface PermissionGuardProps {
  /** Whether the user may perform the action. */
  allowed: boolean;
  /** The policy action, shown in the tooltip (e.g. admin:DeleteUser). */
  action: string;
  /** Renders the control, disabled when the action is not allowed. */
  children: (disabled: boolean) => ReactNode;
}

/**
 * Renders a control disabled, with a tooltip naming the missing permission,
 * when the user may not perform its action (TD-U-09). The wrapper is
 * focusable so that keyboard users can reach the explanation too.
 */
export function PermissionGuard({ allowed, action, children }: PermissionGuardProps) {
  const { t } = useTranslation();
  if (allowed) {
    return <>{children(false)}</>;
  }
  const label = t('permissions.missing', { action });
  return (
    <Tooltip label={label} withArrow>
      <span tabIndex={0} aria-label={label} style={{ display: 'inline-block' }}>
        {children(true)}
      </span>
    </Tooltip>
  );
}
