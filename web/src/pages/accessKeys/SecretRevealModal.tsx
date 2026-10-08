import {
  ActionIcon,
  Alert,
  Button,
  Checkbox,
  Code,
  CopyButton,
  Group,
  Modal,
  Stack,
  Text,
  Tooltip,
} from '@mantine/core';
import { IconAlertTriangle, IconCheck, IconCopy, IconDownload } from '@tabler/icons-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { credentialsDocument } from '@/lib/credentials';
import { saveJsonFile } from '@/lib/saveFile';
import type { CreatedAccessKey } from '@/services';

export interface SecretRevealModalProps {
  /** The key just created; null when the modal is closed. */
  created: CreatedAccessKey | null;
  /** Called on close: the caller must drop the secret from its state. */
  onClose: () => void;
}

function CopyRow({ label, value }: { label: string; value: string }) {
  const { t } = useTranslation();
  return (
    <Stack gap={4}>
      <Text size="sm" fw={500}>
        {label}
      </Text>
      <Group gap="xs" wrap="nowrap">
        <Code style={{ flex: 1, wordBreak: 'break-all' }} aria-label={label}>
          {value}
        </Code>
        <CopyButton value={value}>
          {({ copied, copy }) => (
            <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
              <ActionIcon
                variant="default"
                aria-label={t('accessKeys.secret.copy', { field: label })}
                onClick={copy}
              >
                {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
              </ActionIcon>
            </Tooltip>
          )}
        </CopyButton>
      </Group>
    </Stack>
  );
}

/**
 * Shows the secret of a new access key exactly once (D-IAM-04): copy
 * buttons, download of the credentials as JSON, and an explicit
 * acknowledgement before closing. The secret only lives in the state of
 * the caller until onClose.
 */
export function SecretRevealModal({ created, onClose }: SecretRevealModalProps) {
  const { t } = useTranslation();
  const [acknowledged, setAcknowledged] = useState(false);

  const close = () => {
    setAcknowledged(false);
    onClose();
  };

  return (
    <Modal
      opened={created !== null}
      onClose={close}
      title={t('accessKeys.secret.title')}
      closeOnClickOutside={false}
      closeOnEscape={false}
      withCloseButton={false}
      size="lg"
    >
      {created ? (
        <Stack gap="md">
          <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={18} />} role="alert">
            {t('accessKeys.secret.once')}
          </Alert>
          <CopyRow label={t('accessKeys.fields.accessKey')} value={created.accessKey} />
          <CopyRow label={t('accessKeys.fields.secretKey')} value={created.secretKey} />
          <Group>
            <Button
              variant="light"
              leftSection={<IconDownload size={16} />}
              onClick={() => {
                saveJsonFile(`${created.accessKey}-credentials.json`, credentialsDocument(created));
              }}
            >
              {t('accessKeys.secret.download')}
            </Button>
          </Group>
          <Checkbox
            label={t('accessKeys.secret.acknowledge')}
            checked={acknowledged}
            onChange={(event) => {
              setAcknowledged(event.currentTarget.checked);
            }}
          />
          <Group justify="flex-end">
            <Button onClick={close} disabled={!acknowledged}>
              {t('accessKeys.secret.done')}
            </Button>
          </Group>
        </Stack>
      ) : null}
    </Modal>
  );
}
