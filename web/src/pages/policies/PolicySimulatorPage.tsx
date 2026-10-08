import '@mantine/dates/styles.css';

import {
  ActionIcon,
  Alert,
  Autocomplete,
  Badge,
  Button,
  Code,
  Group,
  Paper,
  SegmentedControl,
  SimpleGrid,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { DateTimePicker } from '@mantine/dates';
import { IconCircleCheck, IconCircleX, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { usePermissions } from '@/auth/usePermissions';
import { useSession } from '@/auth/SessionProvider';
import { ErrorAlert, PageHeader } from '@/components/iam/common';
import { iamKeys } from '@/lib/iamQueries';
import { conditionKeys, policyActions } from '@/lib/policyCatalog';
import { simulateInput, type ConditionRow, type Tls } from '@/lib/simulator';
import {
  useServices,
  type EvaluatedStatement,
  type SimulateInput,
  type Simulation,
} from '@/services';

/** Policy simulator (D-POL-03): user + action + resource + context → decision. */
export function PolicySimulatorPage() {
  const { t } = useTranslation();
  const services = useServices();
  const { session } = useSession();
  const { can } = usePermissions();
  const [user, setUser] = useState(session?.accessKey ?? '');
  const [action, setAction] = useState('s3:GetObject');
  const [bucket, setBucket] = useState('');
  const [object, setObject] = useState('');
  const [sourceIp, setSourceIp] = useState('');
  const [tls, setTls] = useState<Tls>('unset');
  const [time, setTime] = useState<Date | null>(null);
  const [conditions, setConditions] = useState<ConditionRow[]>([]);
  const [nextId, setNextId] = useState(1);

  const users = useQuery({
    queryKey: iamKeys.users,
    queryFn: () => services.users.list(),
    enabled: can('admin:ListUsers'),
  });
  const simulation = useMutation({
    mutationFn: (input: SimulateInput) => services.policies.simulate(input),
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (action.trim() === '') return;
    simulation.mutate(
      simulateInput({ user, action, bucket, object, sourceIp, tls, time, conditions }),
    );
  };

  return (
    <Stack gap="lg">
      <PageHeader title={t('simulator.title')} description={t('simulator.subtitle')} />
      <Paper withBorder p="md" radius="md">
        <form onSubmit={submit} aria-label={t('simulator.title')}>
          <Stack gap="md">
            <SimpleGrid cols={{ base: 1, sm: 2 }}>
              <Autocomplete
                label={t('simulator.user')}
                description={t('simulator.userHint')}
                data={(users.data ?? []).map((u) => u.accessKey)}
                value={user}
                onChange={setUser}
              />
              <Autocomplete
                label={t('simulator.action')}
                data={[...policyActions].filter((a) => !a.includes('*'))}
                value={action}
                onChange={setAction}
                required
                limit={50}
                error={action.trim() === '' ? t('simulator.actionRequired') : undefined}
              />
              <TextInput
                label={t('simulator.bucket')}
                value={bucket}
                onChange={(event) => {
                  setBucket(event.currentTarget.value);
                }}
              />
              <TextInput
                label={t('simulator.object')}
                value={object}
                disabled={bucket.trim() === ''}
                onChange={(event) => {
                  setObject(event.currentTarget.value);
                }}
              />
            </SimpleGrid>
            <Title order={4}>{t('simulator.context')}</Title>
            <SimpleGrid cols={{ base: 1, sm: 3 }}>
              <TextInput
                label={t('simulator.sourceIp')}
                placeholder="203.0.113.10"
                value={sourceIp}
                onChange={(event) => {
                  setSourceIp(event.currentTarget.value);
                }}
              />
              <div>
                <Text size="sm" fw={500} id="simulator-tls" mb={4}>
                  {t('simulator.tls')}
                </Text>
                <SegmentedControl
                  aria-labelledby="simulator-tls"
                  value={tls}
                  onChange={(value) => {
                    setTls(value as Tls);
                  }}
                  data={[
                    { value: 'unset', label: t('simulator.tlsUnset') },
                    { value: 'true', label: t('simulator.tlsOn') },
                    { value: 'false', label: t('simulator.tlsOff') },
                  ]}
                />
              </div>
              <DateTimePicker
                label={t('simulator.time')}
                placeholder={t('simulator.timeNow')}
                value={time}
                onChange={setTime}
                clearable
              />
            </SimpleGrid>
            <Stack gap="xs">
              <Text size="sm" fw={500}>
                {t('simulator.conditions')}
              </Text>
              {conditions.map((row) => (
                <Group key={row.id} gap="xs" align="flex-end" wrap="nowrap">
                  <Autocomplete
                    label={t('simulator.conditionKey')}
                    data={[...conditionKeys]}
                    value={row.key}
                    onChange={(key) => {
                      setConditions((all) => all.map((r) => (r.id === row.id ? { ...r, key } : r)));
                    }}
                    style={{ flex: 1 }}
                  />
                  <TextInput
                    label={t('simulator.conditionValue')}
                    value={row.value}
                    onChange={(event) => {
                      const value = event.currentTarget.value;
                      setConditions((all) =>
                        all.map((r) => (r.id === row.id ? { ...r, value } : r)),
                      );
                    }}
                    style={{ flex: 1 }}
                  />
                  <ActionIcon
                    variant="subtle"
                    color="red"
                    size="lg"
                    aria-label={t('simulator.removeCondition')}
                    onClick={() => {
                      setConditions((all) => all.filter((r) => r.id !== row.id));
                    }}
                  >
                    <IconTrash size={16} />
                  </ActionIcon>
                </Group>
              ))}
              <Group>
                <Button
                  variant="subtle"
                  size="xs"
                  leftSection={<IconPlus size={14} />}
                  onClick={() => {
                    setConditions((all) => [...all, { id: nextId, key: '', value: '' }]);
                    setNextId((n) => n + 1);
                  }}
                >
                  {t('simulator.addCondition')}
                </Button>
              </Group>
            </Stack>
            <Group justify="flex-end">
              <Button type="submit" loading={simulation.isPending}>
                {t('simulator.run')}
              </Button>
            </Group>
          </Stack>
        </form>
      </Paper>
      {simulation.isError ? <ErrorAlert error={simulation.error} /> : null}
      {simulation.data ? <SimulationResult result={simulation.data} /> : null}
    </Stack>
  );
}

type StatementOrigin = Pick<EvaluatedStatement, 'source' | 'policyName' | 'group'>;

/** Describes where a statement comes from ("policy x of group y"). */
function useSourceLabel(): (origin: StatementOrigin) => string {
  const { t } = useTranslation();
  return (origin) => {
    const policy = origin.policyName ?? '';
    switch (origin.source) {
      case 'group':
        return t('simulator.source.group', { policy, group: origin.group ?? '' });
      case 'iam':
        return t('simulator.source.iam', { policy });
      case 'inline':
        return t('simulator.source.inline');
      case 'bucket':
        return t('simulator.source.bucket');
      default:
        return t('simulator.source.root');
    }
  };
}

/** Decision, deciding statement and every evaluated statement. */
export function SimulationResult({ result }: { result: Simulation }) {
  const { t } = useTranslation();
  const sourceLabel = useSourceLabel();
  const allowed = result.allowed;
  return (
    <Stack gap="md" aria-live="polite">
      <Alert
        color={allowed ? 'teal' : 'red'}
        variant="light"
        icon={allowed ? <IconCircleCheck size={20} /> : <IconCircleX size={20} />}
        title={allowed ? t('simulator.allowed') : t('simulator.denied')}
      >
        <Stack gap={4}>
          <Text size="sm">
            {t('simulator.summary', {
              user: result.user,
              action: result.action,
              resource:
                result.bucket !== undefined
                  ? `${result.bucket}${result.object !== undefined ? `/${result.object}` : ''}`
                  : '*',
            })}
          </Text>
          <Text size="sm">{t('simulator.reason', { reason: result.reason })}</Text>
          {result.source !== undefined ? (
            <Text size="sm">
              {t('simulator.decidedBy', {
                source: sourceLabel({
                  source: result.source,
                  ...(result.policyName !== undefined ? { policyName: result.policyName } : {}),
                  ...(result.group !== undefined ? { group: result.group } : {}),
                }),
              })}
              {result.statement?.sid !== undefined ? ` (Sid ${result.statement.sid})` : ''}
            </Text>
          ) : null}
        </Stack>
      </Alert>
      {result.statement?.statement !== undefined ? (
        <Paper withBorder p="sm" radius="md">
          <Text size="sm" fw={500} mb={4}>
            {t('simulator.decidingStatement')}
          </Text>
          <Code block>{JSON.stringify(result.statement.statement, null, 2)}</Code>
        </Paper>
      ) : null}
      <Paper withBorder radius="md">
        <Table.ScrollContainer minWidth={560}>
          <Table verticalSpacing="xs" aria-label={t('simulator.evaluated')}>
            <Table.Caption>{t('simulator.evaluated')}</Table.Caption>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('simulator.columns.source')}</Table.Th>
                <Table.Th>{t('simulator.columns.statement')}</Table.Th>
                <Table.Th>{t('simulator.columns.effect')}</Table.Th>
                <Table.Th>{t('simulator.columns.result')}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {result.evaluated.length === 0 ? (
                <Table.Tr>
                  <Table.Td colSpan={4}>
                    <Text size="sm" c="dimmed">
                      {t('simulator.noStatements')}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {result.evaluated.map((e, i) => {
                const deciding =
                  result.statement !== undefined &&
                  e.source === result.statement.source &&
                  e.policyName === result.statement.policyName &&
                  e.group === result.statement.group &&
                  e.index === result.statement.index;
                return (
                  <Table.Tr
                    key={`${e.source}:${e.policyName ?? ''}:${e.group ?? ''}:${e.index}:${i}`}
                    fw={deciding ? 600 : undefined}
                  >
                    <Table.Td>{sourceLabel(e)}</Table.Td>
                    <Table.Td>
                      #{e.index + 1}
                      {e.sid !== undefined ? ` (${e.sid})` : ''}
                    </Table.Td>
                    <Table.Td>
                      <Badge color={e.effect === 'Allow' ? 'teal' : 'red'} variant="light">
                        {e.effect}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      {e.matched
                        ? t('simulator.matched')
                        : t('simulator.mismatch', {
                            element: t(`simulator.mismatchOf.${e.mismatch ?? 'action'}`),
                          })}
                      {deciding ? ` — ${t('simulator.deciding')}` : ''}
                    </Table.Td>
                  </Table.Tr>
                );
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Paper>
    </Stack>
  );
}
