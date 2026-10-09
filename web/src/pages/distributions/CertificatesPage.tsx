import {
  ActionIcon,
  Badge,
  Button,
  FileButton,
  Group,
  Modal,
  Paper,
  Skeleton,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconFileUpload, IconTrash, IconUpload } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { DistributionActions, usePermissions } from '@/auth/usePermissions';
import { CertificateStatusBadge } from '@/components/distributions/CertificateStatusBadge';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { distributionKeys } from '@/lib/accessQueries';
import { expiryState, expiryWarningDays } from '@/lib/distributions';
import { readTextFile } from '@/lib/saveFile';
import { useServices, type UploadCertificateInput } from '@/services';

import { DistributionsHeader } from './DistributionsHeader';

/** A PEM field with a file picker. */
function PemField({
  label,
  description,
  value,
  onChange,
  placeholder,
}: {
  label: string;
  description: string;
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
}) {
  const { t } = useTranslation();
  return (
    <Stack gap={4}>
      <Textarea
        label={label}
        description={description}
        placeholder={placeholder}
        required
        autosize
        minRows={4}
        maxRows={10}
        value={value}
        spellCheck={false}
        styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 } }}
        onChange={(event) => {
          onChange(event.currentTarget.value);
        }}
      />
      <Group>
        <FileButton
          accept=".pem,.crt,.key,.cer,text/plain"
          onChange={(file) => {
            if (file !== null) void readTextFile(file).then(onChange);
          }}
        >
          {(props) => (
            <Button
              size="xs"
              variant="default"
              leftSection={<IconFileUpload size={14} />}
              {...props}
            >
              {t('certificates.loadFile', { field: label })}
            </Button>
          )}
        </FileButton>
      </Group>
    </Stack>
  );
}

function UploadDialog({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  const { certificates } = useServices();
  const queryClient = useQueryClient();
  const [id, setId] = useState('');
  const [cert, setCert] = useState('');
  const [key, setKey] = useState('');
  const upload = useMutation({
    mutationFn: (input: UploadCertificateInput) => certificates.upload(input),
    onSuccess: async (stored) => {
      await queryClient.invalidateQueries({ queryKey: distributionKeys.certificates });
      notifications.show({
        color: 'green',
        message: t('certificates.uploaded', { id: stored.id }),
      });
      setId('');
      setCert('');
      setKey('');
      onClose();
    },
  });
  return (
    <Modal opened={opened} onClose={onClose} title={t('certificates.uploadTitle')} size="lg">
      <form
        onSubmit={(event) => {
          event.preventDefault();
          upload.mutate({
            ...(id.trim() !== '' ? { id: id.trim() } : {}),
            certificatePem: cert,
            privateKeyPem: key,
          });
        }}
      >
        <Stack gap="sm">
          <TextInput
            label={t('certificates.id')}
            description={t('certificates.idHint')}
            value={id}
            onChange={(event) => {
              setId(event.currentTarget.value);
            }}
          />
          <PemField
            label={t('certificates.certificatePem')}
            description={t('certificates.certificatePemHint')}
            placeholder="-----BEGIN CERTIFICATE-----"
            value={cert}
            onChange={setCert}
          />
          <PemField
            label={t('certificates.privateKeyPem')}
            description={t('certificates.privateKeyPemHint')}
            placeholder="-----BEGIN PRIVATE KEY-----"
            value={key}
            onChange={setKey}
          />
          {upload.isError ? (
            <ErrorAlert error={upload.error} title={t('certificates.invalid')} />
          ) : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button
              type="submit"
              leftSection={<IconUpload size={16} />}
              loading={upload.isPending}
              disabled={cert.trim() === '' || key.trim() === ''}
            >
              {t('certificates.upload')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/**
 * D-DST-07: the TLS certificates of the domains: uploaded (PEM) or
 * obtained by ACME for the distributions with tls.certificate auto, their
 * domains, issuer and expiry (highlighted within 21 days), the
 * distributions using them.
 */
export function CertificatesPage() {
  const { t, i18n } = useTranslation();
  const { certificates } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const canManage = perms.can(DistributionActions.certificates);
  const list = useQuery({
    queryKey: distributionKeys.certificates,
    queryFn: () => certificates.list(),
  });
  const [uploading, setUploading] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);
  const remove = useMutation({
    mutationFn: (id: string) => certificates.remove(id),
    onSuccess: async () => {
      setDeleting(null);
      await queryClient.invalidateQueries({ queryKey: distributionKeys.certificates });
    },
  });
  const locale = i18n.resolvedLanguage;
  const items = list.data?.items ?? [];
  const expiring = items.filter((c) => expiryState(c) !== 'ok');

  return (
    <Stack gap="md">
      <DistributionsHeader
        active="certificates"
        actions={
          <PermissionGuard allowed={canManage} action={DistributionActions.certificates}>
            {(disabled) => (
              <Button
                leftSection={<IconUpload size={16} />}
                disabled={disabled}
                onClick={() => {
                  setUploading(true);
                }}
              >
                {t('certificates.upload')}
              </Button>
            )}
          </PermissionGuard>
        }
      />
      {list.isError ? (
        <ErrorAlert error={list.error} title={t('certificates.loadError')} />
      ) : list.isPending ? (
        <Skeleton height={160} />
      ) : (
        <>
          {expiring.length > 0 ? (
            <Paper
              withBorder
              radius="md"
              p="sm"
              bg="var(--mantine-color-orange-light)"
              role="status"
            >
              <Text size="sm" fw={500}>
                {t('certificates.expiringSummary', {
                  count: expiring.length,
                  days: expiryWarningDays,
                })}
              </Text>
            </Paper>
          ) : null}
          <Paper
            withBorder
            radius="md"
            p="md"
            component="section"
            aria-label={t('certificates.uploadedTitle')}
          >
            <Stack gap="sm">
              <Title order={4}>{t('certificates.uploadedTitle')}</Title>
              {items.length === 0 ? (
                <Text size="sm" c="dimmed">
                  {t('certificates.empty')}
                </Text>
              ) : (
                <Table.ScrollContainer minWidth={820}>
                  <Table aria-label={t('certificates.tableLabel')} verticalSpacing="sm">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t('certificates.id')}</Table.Th>
                        <Table.Th>{t('certificates.domains')}</Table.Th>
                        <Table.Th>{t('certificates.issuer')}</Table.Th>
                        <Table.Th>{t('certificates.notAfter')}</Table.Th>
                        <Table.Th>{t('common.status')}</Table.Th>
                        <Table.Th>{t('certificates.usedBy')}</Table.Th>
                        <Table.Th w={48} />
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {items.map((c) => (
                        <Table.Tr key={c.id} data-expiry={expiryState(c)}>
                          <Table.Td>
                            <Text size="sm" ff="monospace">
                              {c.id}
                            </Text>
                            <Badge size="xs" variant="outline" color="gray">
                              {c.source === 'acme' ? 'ACME' : t('certificates.uploadedBadge')}
                            </Badge>
                          </Table.Td>
                          <Table.Td>
                            <Text size="sm">{c.domains.join(', ')}</Text>
                          </Table.Td>
                          <Table.Td>
                            <Text size="xs">{c.issuer}</Text>
                          </Table.Td>
                          <Table.Td>
                            <Text size="sm">{new Date(c.notAfter).toLocaleDateString(locale)}</Text>
                          </Table.Td>
                          <Table.Td>
                            <CertificateStatusBadge certificate={c} />
                            {c.lastError !== undefined ? (
                              <Text size="xs" c="red">
                                {c.lastError}
                              </Text>
                            ) : null}
                          </Table.Td>
                          <Table.Td>
                            <Text size="sm">{c.usedBy.length > 0 ? c.usedBy.join(', ') : '—'}</Text>
                          </Table.Td>
                          <Table.Td>
                            {canManage ? (
                              <Tooltip label={t('certificates.delete', { id: c.id })}>
                                <ActionIcon
                                  variant="subtle"
                                  color="red"
                                  aria-label={t('certificates.delete', { id: c.id })}
                                  onClick={() => {
                                    remove.reset();
                                    setDeleting(c.id);
                                  }}
                                >
                                  <IconTrash size={16} />
                                </ActionIcon>
                              </Tooltip>
                            ) : null}
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              )}
            </Stack>
          </Paper>
          <Paper
            withBorder
            radius="md"
            p="md"
            component="section"
            aria-label={t('certificates.acmeTitle')}
          >
            <Stack gap="sm">
              <Title order={4}>{t('certificates.acmeTitle')}</Title>
              <Text size="sm" c="dimmed">
                {t('certificates.acmeHint', {
                  directory: list.data.acme.directory === '' ? '—' : list.data.acme.directory,
                  challenge: list.data.acme.challenge,
                })}
              </Text>
              {list.data.acme.managed.length === 0 ? (
                <Text size="sm" c="dimmed">
                  {t('certificates.acmeEmpty')}
                </Text>
              ) : (
                <Table aria-label={t('certificates.acmeTableLabel')} verticalSpacing="xs">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>{t('certificates.domains')}</Table.Th>
                      <Table.Th>{t('certificates.distributions')}</Table.Th>
                      <Table.Th>{t('common.status')}</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {list.data.acme.managed.map((m) => (
                      <Table.Tr key={m.id}>
                        <Table.Td>{m.domains.join(', ')}</Table.Td>
                        <Table.Td>{m.distributions.join(', ')}</Table.Td>
                        <Table.Td>
                          <Group gap={4}>
                            <Badge color={m.state === 'issued' ? 'teal' : 'yellow'} variant="light">
                              {t(`certificates.managedStates.${m.state}`)}
                            </Badge>
                            {m.certificate !== undefined ? (
                              <CertificateStatusBadge certificate={m.certificate} />
                            ) : null}
                          </Group>
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              )}
            </Stack>
          </Paper>
        </>
      )}
      <UploadDialog
        opened={uploading}
        onClose={() => {
          setUploading(false);
        }}
      />
      <ConfirmDialog
        opened={deleting !== null}
        title={t('certificates.deleteTitle')}
        message={t('certificates.deleteConfirm', { id: deleting ?? '' })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setDeleting(null);
        }}
        onConfirm={() => {
          if (deleting !== null) remove.mutate(deleting);
        }}
      />
    </Stack>
  );
}
