import {
  Alert,
  Autocomplete,
  Button,
  Code,
  CopyButton,
  Group,
  NumberInput,
  Paper,
  SegmentedControl,
  Select,
  SimpleGrid,
  Stack,
  Tabs,
  Text,
  TextInput,
} from '@mantine/core';
import { IconCheck, IconCopy } from '@tabler/icons-react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { ErrorAlert } from '@/components/iam/common';
import { expiryPresets, type ExpiryPreset } from '@/lib/expiry';
import { storageKeys } from '@/lib/storageKeys';
import { postSnippets, putSnippets, type Snippets } from '@/lib/uploadSnippets';
import { useServices } from '@/services';

export interface UploadGeneratorProps {
  /** Pre-selected bucket. */
  bucket?: string;
  /** Pre-filled key prefix. */
  prefix?: string;
}

type Mode = 'put' | 'post';
type TypeRule = 'any' | 'exact' | 'prefix';

const mebibyte = 1024 * 1024;

/**
 * Upload link generator (D-SHR-02): a presigned PUT URL for one key, or a
 * POST policy form with conditions (maximum size, key prefix, content
 * type), with copyable curl, HTML and JavaScript snippets.
 */
export function UploadGenerator({ bucket: initialBucket = '', prefix = '' }: UploadGeneratorProps) {
  const { t } = useTranslation();
  const services = useServices();
  const [mode, setMode] = useState<Mode>('post');
  const [bucket, setBucket] = useState(initialBucket);
  const [key, setKey] = useState(prefix);
  const [keyPrefix, setKeyPrefix] = useState(prefix);
  const [expiry, setExpiry] = useState<ExpiryPreset>('1h');
  const [maxSizeMiB, setMaxSizeMiB] = useState<number | string>(10);
  const [typeRule, setTypeRule] = useState<TypeRule>('any');
  const [contentType, setContentType] = useState('');
  const [snippets, setSnippets] = useState<Snippets | null>(null);

  const buckets = useQuery({
    queryKey: storageKeys.buckets,
    queryFn: () => services.buckets.list(),
  });

  const mutation = useMutation({
    mutationFn: async (): Promise<Snippets> => {
      const expiresIn = expiryPresets[expiry];
      if (mode === 'put') {
        const presigned = await services.shares.presign({
          method: 'PUT',
          bucket,
          key,
          expiresIn,
          ...(contentType.trim() !== '' ? { contentType: contentType.trim() } : {}),
        });
        return putSnippets(presigned);
      }
      const size = typeof maxSizeMiB === 'number' ? maxSizeMiB : Number(maxSizeMiB);
      const policy = await services.shares.postPolicy({
        bucket,
        expiresIn,
        ...(keyPrefix.trim() !== '' ? { keyPrefix: keyPrefix.trim() } : {}),
        ...(Number.isFinite(size) && size > 0 ? { maxSize: Math.round(size * mebibyte) } : {}),
        ...(typeRule === 'exact' && contentType.trim() !== ''
          ? { contentType: contentType.trim() }
          : {}),
        ...(typeRule === 'prefix' && contentType.trim() !== ''
          ? { contentTypePrefix: contentType.trim() }
          : {}),
        successActionStatus: 201,
      });
      return postSnippets(policy);
    },
    onSuccess: setSnippets,
  });

  const invalid = bucket.trim() === '' || (mode === 'put' && key.trim() === '');
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!invalid) mutation.mutate();
  };

  return (
    <Stack gap="md">
      <Paper withBorder p="md" radius="md">
        <form onSubmit={submit} noValidate aria-label={t('uploadGenerator.title')}>
          <Stack gap="md">
            <div>
              <Text size="sm" fw={500} id="upload-mode" mb={4}>
                {t('uploadGenerator.mode')}
              </Text>
              <SegmentedControl
                aria-labelledby="upload-mode"
                value={mode}
                onChange={(value) => {
                  setMode(value === 'put' ? 'put' : 'post');
                  setSnippets(null);
                }}
                data={[
                  { value: 'post', label: t('uploadGenerator.post') },
                  { value: 'put', label: t('uploadGenerator.put') },
                ]}
              />
            </div>
            <SimpleGrid cols={{ base: 1, sm: 2 }}>
              <Autocomplete
                label={t('uploadGenerator.bucket')}
                data={(buckets.data ?? []).map((b) => b.name)}
                value={bucket}
                onChange={setBucket}
                required
              />
              {mode === 'put' ? (
                <TextInput
                  label={t('uploadGenerator.key')}
                  required
                  value={key}
                  onChange={(event) => {
                    setKey(event.currentTarget.value);
                  }}
                />
              ) : (
                <TextInput
                  label={t('uploadGenerator.keyPrefix')}
                  description={t('uploadGenerator.keyPrefixHint')}
                  placeholder="uploads/"
                  value={keyPrefix}
                  onChange={(event) => {
                    setKeyPrefix(event.currentTarget.value);
                  }}
                />
              )}
              <Select
                label={t('uploadGenerator.expiry')}
                value={expiry}
                onChange={(value) => {
                  if (value !== null) setExpiry(value as ExpiryPreset);
                }}
                allowDeselect={false}
                data={[
                  { value: '1h', label: t('shareDialog.expiry.1h') },
                  { value: '1d', label: t('shareDialog.expiry.1d') },
                  { value: '7d', label: t('shareDialog.expiry.7d') },
                ]}
              />
              {mode === 'post' ? (
                <NumberInput
                  label={t('uploadGenerator.maxSize')}
                  description={t('uploadGenerator.maxSizeHint')}
                  value={maxSizeMiB}
                  onChange={setMaxSizeMiB}
                  min={0}
                  suffix=" MiB"
                />
              ) : null}
              {mode === 'post' ? (
                <Select
                  label={t('uploadGenerator.typeRule')}
                  value={typeRule}
                  onChange={(value) => {
                    setTypeRule((value as TypeRule | null) ?? 'any');
                  }}
                  allowDeselect={false}
                  data={[
                    { value: 'any', label: t('uploadGenerator.typeAny') },
                    { value: 'exact', label: t('uploadGenerator.typeExact') },
                    { value: 'prefix', label: t('uploadGenerator.typePrefix') },
                  ]}
                />
              ) : null}
              {mode === 'put' || typeRule !== 'any' ? (
                <TextInput
                  label={t('uploadGenerator.contentType')}
                  placeholder={mode === 'post' && typeRule === 'prefix' ? 'image/' : 'image/png'}
                  value={contentType}
                  onChange={(event) => {
                    setContentType(event.currentTarget.value);
                  }}
                />
              ) : null}
            </SimpleGrid>
            <Group justify="flex-end">
              <Button type="submit" disabled={invalid} loading={mutation.isPending}>
                {t('uploadGenerator.generate')}
              </Button>
            </Group>
          </Stack>
        </form>
      </Paper>
      {mutation.isError ? <ErrorAlert error={mutation.error} /> : null}
      {snippets !== null ? <SnippetTabs snippets={snippets} /> : null}
    </Stack>
  );
}

function SnippetTabs({ snippets }: { snippets: Snippets }) {
  const { t } = useTranslation();
  const tabs: Array<[string, string, string]> = [
    ...(snippets.html !== undefined
      ? [['html', t('uploadGenerator.snippets.html'), snippets.html] as [string, string, string]]
      : []),
    ['curl', t('uploadGenerator.snippets.curl'), snippets.curl],
    ['js', t('uploadGenerator.snippets.js'), snippets.javascript],
  ];
  return (
    <Paper
      withBorder
      p="md"
      radius="md"
      component="section"
      aria-label={t('uploadGenerator.snippets.title')}
    >
      <Alert variant="light" mb="sm">
        {t('uploadGenerator.snippets.notice')}
      </Alert>
      <Tabs defaultValue={tabs[0]?.[0] ?? 'curl'}>
        <Tabs.List>
          {tabs.map(([value, label]) => (
            <Tabs.Tab key={value} value={value}>
              {label}
            </Tabs.Tab>
          ))}
        </Tabs.List>
        {tabs.map(([value, label, code]) => (
          <Tabs.Panel key={value} value={value} pt="sm">
            <Stack gap="xs">
              <Group justify="flex-end">
                <CopyButton value={code}>
                  {({ copied, copy }) => (
                    <Button
                      size="xs"
                      variant="default"
                      leftSection={copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
                      onClick={copy}
                      aria-label={t('uploadGenerator.snippets.copy', { kind: label })}
                    >
                      {copied ? t('common.copied') : t('common.copy')}
                    </Button>
                  )}
                </CopyButton>
              </Group>
              <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                {code}
              </Code>
            </Stack>
          </Tabs.Panel>
        ))}
      </Tabs>
    </Paper>
  );
}
