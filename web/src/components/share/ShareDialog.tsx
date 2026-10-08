import '@mantine/dates/styles.css';

import {
  ActionIcon,
  Alert,
  Anchor,
  Button,
  Code,
  CopyButton,
  Group,
  Modal,
  SegmentedControl,
  Select,
  Stack,
  Switch,
  Text,
  TextInput,
  Tooltip,
} from '@mantine/core';
import { DateTimePicker } from '@mantine/dates';
import { IconAlertTriangle, IconCheck, IconCopy, IconLink } from '@tabler/icons-react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { QRCodeSVG } from 'qrcode.react';
import { useMemo, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { ErrorAlert } from '@/components/iam/common';
import {
  checkExpiryDate,
  expiryPresets,
  maxExpirySeconds,
  secondsUntil,
  type ExpiryPreset,
} from '@/lib/expiry';
import { iamKeys } from '@/lib/iamQueries';
import { shellQuote } from '@/lib/uploadSnippets';
import { useServices, type SharePermission } from '@/services';

import { ShareLinks } from './ShareLinks';

export interface ShareDialogProps {
  opened: boolean;
  onClose: () => void;
  bucket: string;
  /** Key of the shared object, or the shared prefix when `prefix` is true. */
  objectKey?: string;
  /**
   * The prefix to share ("photos/2026/"), or true when objectKey names a
   * prefix. Absent or false shares the object objectKey.
   */
  prefix?: string | boolean;
  /** A specific version of the object. */
  versionId?: string;
}

/** What is shared, resolved from the props. */
interface ResolvedTarget {
  bucket: string;
  key: string;
  isPrefix: boolean;
  versionId: string | undefined;
}

function resolveTarget(props: ShareDialogProps): ResolvedTarget {
  if (typeof props.prefix === 'string') {
    return { bucket: props.bucket, key: props.prefix, isPrefix: true, versionId: undefined };
  }
  const isPrefix = props.prefix === true;
  return {
    bucket: props.bucket,
    key: props.objectKey ?? '',
    isPrefix,
    versionId: isPrefix ? undefined : props.versionId,
  };
}

type ExpiryChoice = ExpiryPreset | 'custom';
type Disposition = 'default' | 'inline' | 'attachment';

/** The link produced by the dialog. */
interface ShareResult {
  url: string | undefined;
  expiresAt: string;
  method: string;
  headers: Record<string, string>;
  managed: boolean;
  /**
   * The managed prefix download share whose objects are handed out one
   * link each (the share URL itself only lists the prefix).
   */
  prefixShareId?: string;
}

const baseName = (key: string) => key.split('/').filter(Boolean).pop() ?? key;

/** Builds a Content-Disposition value with an RFC 6266 file name. */
function dispositionValue(kind: Disposition, key: string): string | undefined {
  if (kind === 'default') return undefined;
  const name = baseName(key).replace(/["\\\r\n]/g, '_');
  return `${kind}; filename="${name}"`;
}

/**
 * "Share" dialog of an object, a version or a prefix (D-SHR-01): download
 * or upload permission, validity (presets or a date, at most 7 days),
 * content-disposition, then a managed, revocable link (POST /shares) or,
 * on explicit request, a raw presigned URL that cannot be revoked. The
 * result is shown with copy and QR code.
 */
export function ShareDialog(props: ShareDialogProps) {
  const { opened, onClose } = props;
  const { t } = useTranslation();
  const target = resolveTarget(props);
  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={t('shareDialog.title', { name: `${target.bucket}/${target.key}` })}
      size="lg"
    >
      {opened ? <ShareForm target={target} onClose={onClose} /> : null}
    </Modal>
  );
}

function ShareForm({ target, onClose }: { target: ResolvedTarget; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const { shares } = useServices();
  const queryClient = useQueryClient();
  const { isPrefix, key, versionId } = target;

  const [permission, setPermission] = useState<SharePermission>('download');
  const [expiry, setExpiry] = useState<ExpiryChoice>('1d');
  const [customDate, setCustomDate] = useState<Date | null>(null);
  const [disposition, setDisposition] = useState<Disposition>('default');
  const [contentType, setContentType] = useState('');
  const [raw, setRaw] = useState(false);
  const [result, setResult] = useState<ShareResult | null>(null);
  const now = useMemo(() => new Date(), []);
  const maxDate = useMemo(() => new Date(now.getTime() + maxExpirySeconds * 1000), [now]);

  const expiryProblem = expiry === 'custom' ? checkExpiryDate(customDate) : undefined;
  const expiresIn =
    expiry === 'custom'
      ? customDate !== null
        ? Math.min(secondsUntil(customDate), maxExpirySeconds)
        : 0
      : expiryPresets[expiry];
  const contentDisposition =
    permission === 'download' && !isPrefix ? dispositionValue(disposition, key) : undefined;
  const signedType =
    permission === 'upload' && contentType.trim() !== '' ? contentType.trim() : undefined;

  const mutation = useMutation({
    mutationFn: async (): Promise<ShareResult> => {
      if (raw && !isPrefix) {
        const presigned = await shares.presign({
          method: permission === 'upload' ? 'PUT' : 'GET',
          bucket: target.bucket,
          key,
          expiresIn,
          ...(versionId !== undefined ? { versionId } : {}),
          ...(contentDisposition !== undefined ? { contentDisposition } : {}),
          ...(signedType !== undefined ? { contentType: signedType } : {}),
        });
        return {
          url: presigned.url,
          expiresAt: presigned.expiresAt,
          method: presigned.method,
          headers: presigned.headers,
          managed: false,
        };
      }
      const share = await shares.create({
        bucket: target.bucket,
        key,
        prefix: isPrefix,
        permission,
        expiresIn,
        ...(versionId !== undefined ? { versionId } : {}),
        ...(contentDisposition !== undefined ? { contentDisposition } : {}),
        ...(signedType !== undefined ? { contentType: signedType } : {}),
      });
      return {
        url: share.url,
        expiresAt: share.expiresAt,
        method: permission === 'upload' ? 'PUT' : 'GET',
        headers: signedType !== undefined ? { 'Content-Type': signedType } : {},
        managed: true,
        ...(isPrefix && permission === 'download' ? { prefixShareId: share.id } : {}),
      };
    },
    onSuccess: async (value) => {
      setResult(value);
      if (value.managed) {
        await queryClient.invalidateQueries({ queryKey: iamKeys.shares });
      }
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (expiryProblem !== undefined) return;
    mutation.mutate();
  };

  if (result !== null) {
    return (
      <ShareResultView
        result={result}
        onBack={() => {
          setResult(null);
          mutation.reset();
        }}
        expiresLabel={new Date(result.expiresAt).toLocaleString(i18n.resolvedLanguage, {
          dateStyle: 'medium',
          timeStyle: 'short',
        })}
      />
    );
  }

  const expiryLabels: Record<ExpiryChoice, string> = {
    '1h': t('shareDialog.expiry.1h'),
    '1d': t('shareDialog.expiry.1d'),
    '7d': t('shareDialog.expiry.7d'),
    custom: t('shareDialog.expiry.custom'),
  };

  return (
    <form onSubmit={submit} noValidate aria-label={t('shareDialog.formLabel')}>
      <Stack gap="md">
        {mutation.isError ? <ErrorAlert error={mutation.error} /> : null}
        {versionId !== undefined ? (
          <Text size="sm" c="dimmed">
            {t('shareDialog.version', { version: versionId })}
          </Text>
        ) : null}
        <div>
          <Text size="sm" fw={500} id="share-permission" mb={4}>
            {t('shareDialog.permission')}
          </Text>
          <SegmentedControl
            aria-labelledby="share-permission"
            value={permission}
            onChange={(value) => {
              setPermission(value === 'upload' ? 'upload' : 'download');
            }}
            data={[
              { value: 'download', label: t('shareDialog.download') },
              { value: 'upload', label: t('shareDialog.upload'), disabled: isPrefix },
            ]}
          />
          {isPrefix ? (
            <Text size="xs" c="dimmed" mt={4}>
              {t('shareDialog.prefixUploadHint')}{' '}
              <Anchor component={Link} to="/shares?tab=upload" size="xs">
                {t('shareDialog.uploadGenerator')}
              </Anchor>
            </Text>
          ) : null}
        </div>

        <div>
          <Text size="sm" fw={500} id="share-expiry" mb={4}>
            {t('shareDialog.expiry.label')}
          </Text>
          <SegmentedControl
            aria-labelledby="share-expiry"
            value={expiry}
            onChange={(value) => {
              setExpiry(value as ExpiryChoice);
            }}
            data={(Object.keys(expiryLabels) as ExpiryChoice[]).map((value) => ({
              value,
              label: expiryLabels[value],
            }))}
          />
        </div>
        {expiry === 'custom' ? (
          <DateTimePicker
            label={t('shareDialog.expiry.date')}
            description={t('shareDialog.expiry.max')}
            value={customDate}
            onChange={setCustomDate}
            minDate={now}
            maxDate={maxDate}
            required
            error={
              expiryProblem !== undefined
                ? t(`shareDialog.expiry.problem.${expiryProblem}`)
                : undefined
            }
          />
        ) : null}

        {permission === 'download' && !isPrefix ? (
          <Select
            label={t('shareDialog.disposition.label')}
            description={t('shareDialog.disposition.hint')}
            value={disposition}
            onChange={(value) => {
              setDisposition((value as Disposition | null) ?? 'default');
            }}
            allowDeselect={false}
            data={[
              { value: 'default', label: t('shareDialog.disposition.default') },
              { value: 'inline', label: t('shareDialog.disposition.inline') },
              { value: 'attachment', label: t('shareDialog.disposition.attachment') },
            ]}
          />
        ) : null}
        {permission === 'upload' ? (
          <TextInput
            label={t('shareDialog.contentType')}
            description={t('shareDialog.contentTypeHint')}
            placeholder="application/pdf"
            value={contentType}
            onChange={(event) => {
              setContentType(event.currentTarget.value);
            }}
          />
        ) : null}

        <Switch
          label={t('shareDialog.raw.label')}
          description={t('shareDialog.raw.hint')}
          checked={raw}
          disabled={isPrefix}
          onChange={(event) => {
            setRaw(event.currentTarget.checked);
          }}
        />
        {raw ? (
          <Alert color="orange" variant="light" icon={<IconAlertTriangle size={18} />} role="alert">
            {t('shareDialog.raw.warning')}
          </Alert>
        ) : null}

        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button
            type="submit"
            leftSection={<IconLink size={16} />}
            loading={mutation.isPending}
            disabled={expiryProblem !== undefined}
          >
            {t('shareDialog.create')}
          </Button>
        </Group>
      </Stack>
    </form>
  );
}

function ShareResultView({
  result,
  expiresLabel,
  onBack,
}: {
  result: ShareResult;
  expiresLabel: string;
  onBack: () => void;
}) {
  const { t } = useTranslation();
  const curl =
    result.url !== undefined && result.method === 'PUT'
      ? [
          'curl -X PUT',
          ...Object.entries(result.headers).map(([k, v]) => `  -H ${shellQuote(`${k}: ${v}`)}`),
          '  --upload-file ./file',
          `  ${shellQuote(result.url)}`,
        ].join(' \\\n')
      : undefined;
  return (
    <Stack gap="md">
      <Alert color={result.managed ? 'teal' : 'orange'} variant="light">
        {result.managed ? t('shareDialog.result.managed') : t('shareDialog.result.raw')}
      </Alert>
      <Text size="sm">{t('shareDialog.result.expires', { date: expiresLabel })}</Text>
      {result.prefixShareId !== undefined ? (
        <ShareLinks shareId={result.prefixShareId} />
      ) : result.url !== undefined ? (
        <>
          <Group gap="xs" align="flex-end" wrap="nowrap">
            <TextInput
              label={t('shareDialog.result.url')}
              value={result.url}
              readOnly
              style={{ flex: 1 }}
              onFocus={(event) => {
                event.currentTarget.select();
              }}
            />
            <CopyButton value={result.url}>
              {({ copied, copy }) => (
                <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
                  <ActionIcon
                    variant="default"
                    size="lg"
                    aria-label={t('shareDialog.result.copy')}
                    onClick={copy}
                  >
                    {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                  </ActionIcon>
                </Tooltip>
              )}
            </CopyButton>
          </Group>
          <Group justify="center">
            <QRCodeSVG
              value={result.url}
              size={176}
              level="M"
              marginSize={2}
              title={t('shareDialog.result.qr')}
              role="img"
              aria-label={t('shareDialog.result.qr')}
              style={{ background: 'white' }}
            />
          </Group>
          {curl !== undefined ? (
            <Stack gap={4}>
              <Text size="sm" fw={500}>
                {t('shareDialog.result.curl')}
              </Text>
              <Code block>{curl}</Code>
            </Stack>
          ) : null}
        </>
      ) : (
        <Text size="sm" c="dimmed">
          {t('shareDialog.result.noUrl')}
        </Text>
      )}
      <Group justify="flex-end">
        <Button variant="default" onClick={onBack}>
          {t('shareDialog.result.another')}
        </Button>
      </Group>
    </Stack>
  );
}
