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
import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { saveTextFile } from '@/lib/saveFile';

export interface RevealedSecret {
  label: string;
  value: string;
}

export interface RevealOnceModalProps {
  title: string;
  /** What the secret is for, under the warning. */
  description?: ReactNode;
  /** The secrets to show; null closes the modal. */
  secrets: readonly RevealedSecret[] | null;
  /** The file offered for download (name and text). */
  download?: { filename: string; text: string; type?: string } | undefined;
  onClose: () => void;
}

/**
 * Shows secrets the server returns once and never stores in clear (a
 * private key of a key group, an origin secret): copy buttons, a download
 * and an explicit acknowledgement before closing. The caller drops the
 * secret from its state on close.
 */
export function RevealOnceModal({
  title,
  description,
  secrets,
  download,
  onClose,
}: RevealOnceModalProps) {
  const { t } = useTranslation();
  const [acknowledged, setAcknowledged] = useState(false);
  const close = () => {
    setAcknowledged(false);
    onClose();
  };
  return (
    <Modal
      opened={secrets !== null}
      onClose={close}
      title={title}
      closeOnClickOutside={false}
      closeOnEscape={false}
      withCloseButton={false}
      size="lg"
    >
      {secrets !== null ? (
        <Stack gap="md">
          <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={18} />} role="alert">
            {t('revealOnce.warning')}
          </Alert>
          {description !== undefined ? <Text size="sm">{description}</Text> : null}
          {secrets.map((s) => (
            <Stack key={s.label} gap={4}>
              <Text size="sm" fw={500}>
                {s.label}
              </Text>
              <Group gap="xs" wrap="nowrap" align="flex-start">
                <Code
                  block
                  aria-label={s.label}
                  style={{
                    flex: 1,
                    whiteSpace: 'pre-wrap',
                    wordBreak: 'break-all',
                    maxHeight: 220,
                    overflow: 'auto',
                  }}
                >
                  {s.value}
                </Code>
                <CopyButton value={s.value}>
                  {({ copied, copy }) => (
                    <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
                      <ActionIcon
                        variant="default"
                        aria-label={t('revealOnce.copy', { label: s.label })}
                        onClick={copy}
                      >
                        {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                      </ActionIcon>
                    </Tooltip>
                  )}
                </CopyButton>
              </Group>
            </Stack>
          ))}
          {download !== undefined ? (
            <Group>
              <Button
                variant="light"
                leftSection={<IconDownload size={16} />}
                onClick={() => {
                  saveTextFile(download.filename, download.text, download.type ?? 'text/plain');
                }}
              >
                {t('revealOnce.download', { filename: download.filename })}
              </Button>
            </Group>
          ) : null}
          <Checkbox
            label={t('revealOnce.acknowledge')}
            checked={acknowledged}
            onChange={(event) => {
              setAcknowledged(event.currentTarget.checked);
            }}
          />
          <Group justify="flex-end">
            <Button onClick={close} disabled={!acknowledged}>
              {t('revealOnce.done')}
            </Button>
          </Group>
        </Stack>
      ) : null}
    </Modal>
  );
}
