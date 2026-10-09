import {
  Alert,
  Badge,
  Button,
  Code,
  Group,
  Paper,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconPlayerPlay } from '@tabler/icons-react';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { patternLabel } from '@/lib/distributions';
import { problemMessage } from '@/lib/problem';
import { useServices, type Distribution, type DistributionTestResult } from '@/services';

export interface UrlTesterProps {
  distribution: Distribution;
  /** The draft differs from the stored distribution, which the server tests. */
  dirty: boolean;
  /** Called with the index of the behavior serving the URL (-1: default). */
  onResult?: (behaviorIndex: number | null) => void;
}

/**
 * D-DST-03: "which behavior serves this URL?". The server plans the
 * request on the stored distribution without reading the object: the
 * route of the host, the behavior with its inherited fields, the object
 * key, a redirection or a refusal, and the headers of the response.
 */
export function UrlTester({ distribution, dirty, onResult }: UrlTesterProps) {
  const { t } = useTranslation();
  const { distributions } = useServices();
  const firstAlias = distribution.aliases.find((a) => !a.startsWith('*.')) ?? '';
  const [url, setUrl] = useState(firstAlias === '' ? '/' : `https://${firstAlias}/`);
  const [method, setMethod] = useState('GET');
  const [clientIp, setClientIp] = useState('');
  const test = useMutation({
    mutationFn: () =>
      distributions.test(distribution.id, {
        url: url.trim(),
        method,
        ...(clientIp.trim() !== '' ? { clientIp: clientIp.trim() } : {}),
      }),
    onSuccess: (result) => {
      onResult?.(result.behaviorIndex);
    },
    onError: () => {
      onResult?.(null);
    },
  });

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('distributions.tester.title')}
    >
      <Stack gap="sm">
        <Title order={4}>{t('distributions.tester.title')}</Title>
        <Text size="sm" c="dimmed">
          {t('distributions.tester.description')}
        </Text>
        {dirty ? (
          <Alert color="yellow" variant="light">
            {t('distributions.tester.unsaved')}
          </Alert>
        ) : null}
        <form
          onSubmit={(event) => {
            event.preventDefault();
            test.mutate();
          }}
        >
          <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
            <TextInput
              label={t('distributions.tester.url')}
              value={url}
              onChange={(event) => {
                setUrl(event.currentTarget.value);
              }}
              style={{ gridColumn: 'span 1' }}
            />
            <Select
              label={t('distributions.tester.method')}
              data={['GET', 'HEAD', 'OPTIONS', 'POST', 'PUT', 'DELETE']}
              value={method}
              allowDeselect={false}
              onChange={(value) => {
                if (value !== null) setMethod(value);
              }}
            />
            <TextInput
              label={t('distributions.tester.clientIp')}
              placeholder="203.0.113.7"
              value={clientIp}
              onChange={(event) => {
                setClientIp(event.currentTarget.value);
              }}
            />
          </SimpleGrid>
          <Group mt="sm">
            <Button
              type="submit"
              leftSection={<IconPlayerPlay size={16} />}
              loading={test.isPending}
            >
              {t('distributions.tester.run')}
            </Button>
          </Group>
        </form>
        {test.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, test.error)}
          </Alert>
        ) : null}
        {test.data !== undefined ? (
          <TestResult distribution={distribution} result={test.data} />
        ) : null}
      </Stack>
    </Paper>
  );
}

function TestResult({
  distribution,
  result,
}: {
  distribution: Distribution;
  result: DistributionTestResult;
}) {
  const { t } = useTranslation();
  const behavior = result.behavior;
  const pattern =
    result.behaviorIndex >= 0
      ? distribution.behaviors?.[result.behaviorIndex]?.pathPattern
      : undefined;
  const headers = { ...result.responseHeaders };
  if (behavior.cache?.cacheControl !== undefined && headers['Cache-Control'] === undefined) {
    headers['Cache-Control'] = behavior.cache.cacheControl;
  }
  return (
    <Stack gap="xs" aria-live="polite" aria-label={t('distributions.tester.result')} role="region">
      <Group gap="xs" wrap="wrap">
        <Badge variant="light">{t(`dns.kinds.${result.route.kind}`)}</Badge>
        {result.refusalStatus !== undefined ? (
          <Badge color="red.9">
            {t('distributions.tester.refused', { status: result.refusalStatus })}
          </Badge>
        ) : result.redirectStatus !== undefined ? (
          <Badge color="blue">
            {t('distributions.tester.redirected', { status: result.redirectStatus })}
          </Badge>
        ) : (
          <Badge color="teal.9">{t('distributions.tester.served')}</Badge>
        )}
      </Group>
      <Text size="sm">
        {result.behaviorIndex < 0
          ? t('distributions.tester.defaultBehavior')
          : t('distributions.tester.behavior', {
              n: result.behaviorIndex + 1,
              pattern: patternLabel(pattern),
            })}
      </Text>
      <Text size="sm">
        {t('distributions.tester.effective', {
          access: t(`distributions.access.${behavior.access ?? 'public'}`),
          methods: (behavior.allowedMethods ?? []).join(', '),
        })}
      </Text>
      {result.key !== undefined ? (
        <Text size="sm">
          {t('distributions.tester.object')} <Code>{`${result.bucket ?? ''}/${result.key}`}</Code>
        </Text>
      ) : null}
      {result.redirectLocation !== undefined ? (
        <Text size="sm">
          {t('distributions.tester.location')} <Code>{result.redirectLocation}</Code>
        </Text>
      ) : null}
      {result.refusal !== undefined ? (
        <Text size="sm" c="red">
          {result.refusal}
        </Text>
      ) : null}
      {Object.keys(headers).length > 0 ? (
        <Table aria-label={t('distributions.tester.headers')} verticalSpacing={2} withTableBorder>
          <Table.Tbody>
            {Object.entries(headers).map(([name, value]) => (
              <Table.Tr key={name}>
                <Table.Td>
                  <Text size="sm" ff="monospace">
                    {name}
                  </Text>
                </Table.Td>
                <Table.Td>
                  <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                    {value}
                  </Text>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      ) : null}
    </Stack>
  );
}
