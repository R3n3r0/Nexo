import {
  Alert,
  Badge,
  Button,
  Group,
  List,
  Paper,
  Select,
  SimpleGrid,
  Stack,
  Text,
  Textarea,
  TextInput,
  Title,
} from '@mantine/core';
import { IconListSearch, IconPlayerPlay } from '@tabler/icons-react';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { problemMessage } from '@/lib/problem';
import {
  useServices,
  type AccessRulesKeyResult,
  type AccessRulesTestResult,
  type TestAccessRulesInput,
} from '@/services';

export interface RuleTesterProps {
  bucket: string;
  /** Called with the result chosen for the highlight of the rules table. */
  onSelect: (result: AccessRulesKeyResult | null) => void;
  /** The key result highlighted in the table. */
  selected: string | null;
}

const testActions = ['read', 'list', 'write', 'delete', 'tag'] as const;

/**
 * D-ACC-02: the pattern tester. Keys (typed or suggested from the bucket)
 * are evaluated by the server with every source of the policy engine for
 * an identity, an address, a host: the rules each key matches, the
 * outcome of the rules and the final decision with the rule that
 * determined it.
 */
export function RuleTester({ bucket, onSelect, selected }: RuleTesterProps) {
  const { t } = useTranslation();
  const { accessRules, objects } = useServices();
  const [keysText, setKeysText] = useState('');
  const [principal, setPrincipal] = useState('anonymous');
  const [action, setAction] = useState<string>('read');
  const [ip, setIp] = useState('');
  const [host, setHost] = useState('');
  const [referer, setReferer] = useState('');
  const [secure, setSecure] = useState<'unknown' | 'https' | 'http'>('unknown');

  const keys = keysText
    .split('\n')
    .map((k) => k.trim())
    .filter((k) => k !== '');

  const suggest = useMutation({
    mutationFn: async () => {
      const page = await objects.list(bucket, { delimiter: '', limit: 20 });
      return page.objects.map((o) => o.key);
    },
    onSuccess: (found) => {
      const merged = [...new Set([...keys, ...found])];
      setKeysText(merged.join('\n'));
    },
  });

  const run = useMutation({
    mutationFn: () => {
      const input: TestAccessRulesInput = {
        keys,
        action,
        principal: principal.trim() === '' ? 'anonymous' : principal.trim(),
        ...(ip.trim() !== '' ? { ip: ip.trim() } : {}),
        ...(host.trim() !== '' ? { host: host.trim() } : {}),
        ...(referer.trim() !== '' ? { referer: referer.trim() } : {}),
        ...(secure !== 'unknown' ? { secure: secure === 'https' } : {}),
      };
      return accessRules.test(bucket, input);
    },
    onSuccess: (result: AccessRulesTestResult) => {
      onSelect(result.results[0] ?? null);
    },
  });

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('accessRules.tester.title')}
    >
      <Stack gap="sm">
        <Stack gap={2}>
          <Title order={4}>{t('accessRules.tester.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('accessRules.tester.description')}
          </Text>
        </Stack>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            run.mutate();
          }}
        >
          <Stack gap="sm">
            <Textarea
              label={t('accessRules.tester.keys')}
              description={t('accessRules.tester.keysHint')}
              autosize
              minRows={3}
              maxRows={10}
              value={keysText}
              spellCheck={false}
              styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
              onChange={(event) => {
                setKeysText(event.currentTarget.value);
              }}
            />
            <Group gap="xs">
              <Button
                size="xs"
                variant="default"
                leftSection={<IconListSearch size={14} />}
                loading={suggest.isPending}
                onClick={() => {
                  suggest.mutate();
                }}
              >
                {t('accessRules.tester.suggest')}
              </Button>
              {suggest.isError ? (
                <Text size="xs" c="red">
                  {problemMessage(t, suggest.error)}
                </Text>
              ) : null}
            </Group>
            <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm">
              <TextInput
                label={t('accessRules.tester.principal')}
                description={t('accessRules.tester.principalHint')}
                value={principal}
                onChange={(event) => {
                  setPrincipal(event.currentTarget.value);
                }}
              />
              <Select
                label={t('accessRules.tester.action')}
                data={testActions.map((a) => ({ value: a, label: t(`accessRules.actions.${a}`) }))}
                value={action}
                allowDeselect={false}
                onChange={(value) => {
                  if (value !== null) setAction(value);
                }}
              />
              <TextInput
                label={t('accessRules.tester.ip')}
                placeholder="203.0.113.7"
                value={ip}
                onChange={(event) => {
                  setIp(event.currentTarget.value);
                }}
              />
              <TextInput
                label={t('accessRules.tester.host')}
                placeholder="cdn.example.com"
                value={host}
                onChange={(event) => {
                  setHost(event.currentTarget.value);
                }}
              />
              <TextInput
                label={t('accessRules.tester.referer')}
                placeholder="https://www.example.com/"
                value={referer}
                onChange={(event) => {
                  setReferer(event.currentTarget.value);
                }}
              />
              <Select
                label={t('accessRules.tester.transport')}
                data={(['unknown', 'https', 'http'] as const).map((v) => ({
                  value: v,
                  label: t(`accessRules.tester.transports.${v}`),
                }))}
                value={secure}
                allowDeselect={false}
                onChange={(value) => {
                  if (value === 'https' || value === 'http' || value === 'unknown')
                    setSecure(value);
                }}
              />
            </SimpleGrid>
            <Group>
              <Button
                type="submit"
                leftSection={<IconPlayerPlay size={16} />}
                loading={run.isPending}
              >
                {t('accessRules.tester.run')}
              </Button>
            </Group>
          </Stack>
        </form>
        {run.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, run.error)}
          </Alert>
        ) : null}
        {run.data !== undefined ? (
          <TesterResults result={run.data} selected={selected} onSelect={onSelect} />
        ) : null}
      </Stack>
    </Paper>
  );
}

function TesterResults({
  result,
  selected,
  onSelect,
}: {
  result: AccessRulesTestResult;
  selected: string | null;
  onSelect: (result: AccessRulesKeyResult) => void;
}) {
  const { t } = useTranslation();
  return (
    <Stack gap="xs" aria-live="polite">
      <Group gap="xs">
        <Text size="sm">{t('accessRules.tester.evaluated', { action: result.action })}</Text>
        {result.blockPublic.effective ? (
          <Badge color="grape" variant="light">
            {t('blockPublic.active')}
          </Badge>
        ) : null}
      </Group>
      <List listStyleType="none" spacing="xs" aria-label={t('accessRules.tester.results')}>
        {result.results.map((r) => (
          <List.Item
            key={r.key}
            styles={{ itemWrapper: { width: '100%' }, itemLabel: { width: '100%' } }}
          >
            <Paper
              withBorder
              p="sm"
              radius="sm"
              style={{
                borderColor: selected === r.key ? 'var(--mantine-color-blue-6)' : undefined,
              }}
              data-key={r.key}
            >
              <Stack gap={6}>
                <Group justify="space-between" wrap="wrap" gap="xs">
                  <Group gap="xs" wrap="wrap">
                    <Text size="sm" ff="monospace" fw={600} style={{ overflowWrap: 'anywhere' }}>
                      {r.key === '' ? t('accessRules.tester.bucketItself') : r.key}
                    </Text>
                    {r.normalizedKey !== r.key ? (
                      <Text size="xs" c="dimmed" ff="monospace">
                        → {r.normalizedKey}
                      </Text>
                    ) : null}
                  </Group>
                  <Group gap={4}>
                    <Badge variant="outline" color="gray">
                      {t('accessRules.tester.rulesOutcome', {
                        outcome: t(`accessRules.tester.outcomes.${r.rulesOutcome}`),
                      })}
                    </Badge>
                    <Badge color={r.allowed ? 'teal.9' : 'red.9'}>
                      {r.allowed ? t('accessRules.tester.allowed') : t('accessRules.tester.denied')}
                    </Badge>
                    <Button
                      size="compact-xs"
                      variant={selected === r.key ? 'filled' : 'default'}
                      aria-pressed={selected === r.key}
                      aria-label={t('accessRules.tester.highlight', {
                        key: r.key === '' ? t('accessRules.tester.bucketItself') : r.key,
                      })}
                      onClick={() => {
                        onSelect(r);
                      }}
                    >
                      {t('accessRules.tester.show')}
                    </Button>
                  </Group>
                </Group>
                <Group gap={4} wrap="wrap">
                  {r.rules.map((rule) => (
                    <Badge
                      key={rule.id}
                      size="sm"
                      tt="none"
                      variant={rule.matched ? 'filled' : 'outline'}
                      color={
                        r.determinant === rule.id
                          ? 'blue'
                          : rule.matched
                            ? rule.effect === 'Deny'
                              ? 'red.9'
                              : 'teal.9'
                            : 'gray'
                      }
                      title={
                        rule.matched
                          ? t('accessRules.tester.applies')
                          : t(`accessRules.tester.mismatch.${rule.mismatch ?? 'key'}`, {
                              condition: rule.condition ?? '',
                            })
                      }
                    >
                      {rule.id}
                      {rule.matched
                        ? ''
                        : ` · ${t(`accessRules.tester.mismatchShort.${rule.mismatch ?? 'key'}`)}`}
                    </Badge>
                  ))}
                  {r.rules.length === 0 ? (
                    <Text size="xs" c="dimmed">
                      {t('accessRules.tester.noRules')}
                    </Text>
                  ) : null}
                </Group>
                <Text size="xs" c="dimmed">
                  {r.determinant !== undefined
                    ? t('accessRules.tester.determinant', { id: r.determinant })
                    : r.source !== undefined && r.source !== ''
                      ? t('accessRules.tester.source', { source: r.source })
                      : ''}{' '}
                  {r.reason}
                </Text>
              </Stack>
            </Paper>
          </List.Item>
        ))}
      </List>
    </Stack>
  );
}
