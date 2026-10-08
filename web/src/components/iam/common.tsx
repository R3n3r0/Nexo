import { Alert, Badge, Button, Group, Modal, Stack, Text, Title } from '@mantine/core';
import { IconAlertCircle } from '@tabler/icons-react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { errorText } from '@/lib/errorText';
import type { CredentialStatus } from '@/services';

export interface PageHeaderProps {
  title: string;
  description?: string;
  /** Buttons on the right (create, export...). */
  actions?: ReactNode;
}

/** Title, subtitle and primary actions of an IAM page. */
export function PageHeader({ title, description, actions }: PageHeaderProps) {
  return (
    <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
      <Stack gap={2}>
        <Title order={2}>{title}</Title>
        {description !== undefined ? (
          <Text c="dimmed" size="sm">
            {description}
          </Text>
        ) : null}
      </Stack>
      {actions !== undefined ? <Group gap="xs">{actions}</Group> : null}
    </Group>
  );
}

/** Enabled/disabled badge of users, groups and access keys. */
export function StatusBadge({ status }: { status: CredentialStatus }) {
  const { t } = useTranslation();
  return (
    <Badge color={status === 'enabled' ? 'teal' : 'gray'} variant="light">
      {t(`iam.status.${status}`)}
    </Badge>
  );
}

export interface ErrorAlertProps {
  error: unknown;
  /** Lead sentence ("Could not load the users."). */
  title?: string;
  onRetry?: () => void;
}

/** A failure with the problem detail and an optional retry button. */
export function ErrorAlert({ error, title, onRetry }: ErrorAlertProps) {
  const { t } = useTranslation();
  return (
    <Alert
      color="red"
      variant="light"
      icon={<IconAlertCircle size={18} />}
      role="alert"
      title={title}
    >
      <Group justify="space-between" wrap="nowrap" gap="sm">
        <Text size="sm">{errorText(error, t('common.unknownError'))}</Text>
        {onRetry !== undefined ? (
          <Button size="xs" variant="default" onClick={onRetry}>
            {t('common.retry')}
          </Button>
        ) : null}
      </Group>
    </Alert>
  );
}

export interface ConfirmDialogProps {
  opened: boolean;
  title: string;
  message: ReactNode;
  confirmLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
  loading?: boolean;
  /** Error of the last attempt, shown in the dialog. */
  error?: unknown;
  danger?: boolean;
}

/** Confirmation of a destructive action (delete, revoke). */
export function ConfirmDialog({
  opened,
  title,
  message,
  confirmLabel,
  onConfirm,
  onCancel,
  loading = false,
  error,
  danger = true,
}: ConfirmDialogProps) {
  const { t } = useTranslation();
  return (
    <Modal opened={opened} onClose={onCancel} title={title} centered>
      <Stack gap="md">
        <Text size="sm">{message}</Text>
        {error !== undefined && error !== null ? <ErrorAlert error={error} /> : null}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onCancel} disabled={loading}>
            {t('common.cancel')}
          </Button>
          <Button color={danger ? 'red' : undefined} onClick={onConfirm} loading={loading}>
            {confirmLabel}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
