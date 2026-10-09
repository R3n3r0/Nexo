import {
  ActionIcon,
  Alert,
  Anchor,
  Button,
  Code,
  CopyButton,
  FileButton,
  Group,
  Paper,
  Select,
  SimpleGrid,
  Stack,
  Switch,
  Table,
  Text,
  Textarea,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { IconCheck, IconCopy, IconFileUpload, IconSignature } from '@tabler/icons-react';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { problemMessage } from '@/lib/problem';
import { readTextFile } from '@/lib/saveFile';
import { useServices, type Distribution, type SignDistributionInput } from '@/services';

export interface SignedUrlGeneratorProps {
  distribution: Distribution;
  /** Key pair ids of the key groups of the signed behaviors. */
  keyPairs: readonly { group: string; id: string }[];
}

const expiries = [
  { value: '3600', seconds: 3600 },
  { value: '86400', seconds: 86400 },
  { value: '604800', seconds: 604800 },
] as const;

/**
 * D-DST-05: signed URLs or signed cookies of a signed behavior, with the
 * private key of a key of its key group. The key is sent to the server for
 * this signature only (never stored); a custom policy limits the start
 * date, the viewer address or the resource (wildcards).
 */
export function SignedUrlGenerator({ distribution, keyPairs }: SignedUrlGeneratorProps) {
  const { t } = useTranslation();
  const { distributions } = useServices();
  const firstAlias = distribution.aliases.find((a) => !a.startsWith('*.')) ?? '';
  const [url, setUrl] = useState('/');
  const [privateKey, setPrivateKey] = useState('');
  const [keyPairId, setKeyPairId] = useState<string | null>(null);
  const [expiry, setExpiry] = useState<string>('3600');
  const [ipRange, setIpRange] = useState('');
  const [resource, setResource] = useState('');
  const [cookies, setCookies] = useState(false);

  const sign = useMutation({
    mutationFn: () => {
      const input: SignDistributionInput = {
        url: url.trim(),
        privateKeyPem: privateKey,
        expiresIn: Number(expiry),
        ...(keyPairId !== null ? { keyPairId } : {}),
        ...(ipRange.trim() !== '' ? { ipRange: ipRange.trim() } : {}),
        ...(resource.trim() !== '' ? { resource: resource.trim() } : {}),
        ...(cookies ? { cookies: true } : {}),
      };
      return distributions.sign(distribution.id, input);
    },
  });

  const result = sign.data;
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('signing.title')}>
      <Stack gap="sm">
        <Title order={4}>{t('signing.title')}</Title>
        <Text size="sm" c="dimmed">
          {t('signing.description')}
        </Text>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            sign.mutate();
          }}
        >
          <Stack gap="sm">
            <TextInput
              label={t('signing.url')}
              description={t('signing.urlHint', { host: firstAlias })}
              value={url}
              required
              onChange={(event) => {
                setUrl(event.currentTarget.value);
              }}
            />
            <Textarea
              label={t('signing.privateKey')}
              description={t('signing.privateKeyHint')}
              placeholder="-----BEGIN PRIVATE KEY-----"
              autosize
              minRows={3}
              maxRows={8}
              required
              value={privateKey}
              spellCheck={false}
              styles={{
                input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 },
              }}
              onChange={(event) => {
                setPrivateKey(event.currentTarget.value);
              }}
            />
            <Group>
              <FileButton
                accept=".pem,.key,text/plain"
                onChange={(file) => {
                  if (file !== null) void readTextFile(file).then(setPrivateKey);
                }}
              >
                {(props) => (
                  <Button
                    size="xs"
                    variant="default"
                    leftSection={<IconFileUpload size={14} />}
                    {...props}
                  >
                    {t('signing.loadKey')}
                  </Button>
                )}
              </FileButton>
            </Group>
            <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
              <Select
                label={t('signing.keyPair')}
                placeholder={t('signing.keyPairAuto')}
                data={keyPairs.map((k) => ({ value: k.id, label: `${k.id} (${k.group})` }))}
                value={keyPairId}
                clearable
                onChange={setKeyPairId}
              />
              <Select
                label={t('signing.expiry')}
                data={expiries.map((e) => ({
                  value: e.value,
                  label: t(`signing.expiries.${e.value}`),
                }))}
                value={expiry}
                allowDeselect={false}
                onChange={(value) => {
                  if (value !== null) setExpiry(value);
                }}
              />
              <TextInput
                label={t('signing.ipRange')}
                placeholder="203.0.113.0/24"
                value={ipRange}
                onChange={(event) => {
                  setIpRange(event.currentTarget.value);
                }}
              />
              <TextInput
                label={t('signing.resource')}
                description={t('signing.resourceHint')}
                placeholder={`https://${firstAlias}/premium/*`}
                value={resource}
                onChange={(event) => {
                  setResource(event.currentTarget.value);
                }}
              />
            </SimpleGrid>
            <Switch
              label={t('signing.cookies')}
              description={t('signing.cookiesHint')}
              checked={cookies}
              onChange={(event) => {
                setCookies(event.currentTarget.checked);
              }}
            />
            <Group>
              <Button
                type="submit"
                leftSection={<IconSignature size={16} />}
                loading={sign.isPending}
                disabled={privateKey.trim() === '' || url.trim() === ''}
              >
                {cookies ? t('signing.signCookies') : t('signing.signUrl')}
              </Button>
            </Group>
          </Stack>
        </form>
        {sign.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, sign.error)}
          </Alert>
        ) : null}
        {result !== undefined ? (
          <Stack gap="xs" aria-live="polite" role="region" aria-label={t('signing.result')}>
            <Text size="sm">
              {t('signing.expires', {
                date: new Date(result.expiresAt).toLocaleString(),
                key: result.keyPairId,
                group: result.keyGroup,
              })}
            </Text>
            {result.cookies.length === 0 ? (
              <Group gap="xs" wrap="nowrap" align="flex-start">
                <Code
                  block
                  style={{ flex: 1, wordBreak: 'break-all', whiteSpace: 'pre-wrap' }}
                  aria-label={t('signing.signedUrl')}
                >
                  {result.url}
                </Code>
                <CopyButton value={result.url}>
                  {({ copied, copy }) => (
                    <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
                      <ActionIcon
                        variant="default"
                        aria-label={t('signing.copyUrl')}
                        onClick={copy}
                      >
                        {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                      </ActionIcon>
                    </Tooltip>
                  )}
                </CopyButton>
              </Group>
            ) : (
              <Table aria-label={t('signing.cookieTable')} verticalSpacing={2} withTableBorder>
                <Table.Tbody>
                  {result.cookies.map((c) => (
                    <Table.Tr key={c.name}>
                      <Table.Td>
                        <Text size="sm" ff="monospace">
                          {c.name}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Text size="xs" ff="monospace" style={{ wordBreak: 'break-all' }}>
                          {c.value}
                        </Text>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}
            {result.cookies.length === 0 ? (
              <Anchor href={result.url} target="_blank" rel="noopener noreferrer" size="sm">
                {t('signing.open')}
              </Anchor>
            ) : null}
          </Stack>
        ) : null}
      </Stack>
    </Paper>
  );
}
