import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Checkbox,
  Code,
  CopyButton,
  Group,
  Modal,
  Stack,
  Stepper,
  Table,
  Text,
  TextInput,
  Tooltip,
} from '@mantine/core';
import { IconAlertTriangle, IconCheck, IconCopy, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { MaintenanceActions, usePermissions } from '@/auth/usePermissions';
import { ErrorAlert } from '@/components/iam/common';
import { useServices, type PoolPlan, type Pools } from '@/services';

/** Quotes an argument for a POSIX shell. */
const quote = (arg: string) => `'${arg.replaceAll("'", `'\\''`)}'`;

/** A command line with a button that copies it. */
function CommandLine({ label, value }: { label: string; value: string }) {
  const { t } = useTranslation();
  return (
    <Stack gap={4}>
      <Text size="sm" fw={600}>
        {label}
      </Text>
      <Group gap="xs" wrap="nowrap" align="flex-start">
        <Code block style={{ flex: 1, overflowWrap: 'anywhere', whiteSpace: 'pre-wrap' }}>
          {value}
        </Code>
        <CopyButton value={value}>
          {({ copied, copy }) => (
            <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
              <ActionIcon
                variant="default"
                size="lg"
                aria-label={`${t('common.copy')}: ${label}`}
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

/** The planned pools: kept, new and removed. */
function PlanTable({ plan }: { plan: PoolPlan }) {
  const { t } = useTranslation();
  return (
    <Table.ScrollContainer minWidth={640}>
      <Table aria-label={t('clusterExpand.planned')} verticalSpacing={4}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t('clusterPools.pool')}</Table.Th>
            <Table.Th>{t('clusterExpand.argument')}</Table.Th>
            <Table.Th>{t('clusterExpand.layout')}</Table.Th>
            <Table.Th>{t('common.status')}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {plan.pools.map((p) => (
            <Table.Tr key={p.index}>
              <Table.Td>{p.index}</Table.Td>
              <Table.Td>
                <Text size="xs" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                  {p.arg}
                </Text>
                <Text size="xs" c="dimmed">
                  {p.nodes.join(', ')}
                </Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm">
                  {t('clusterExpand.sets', {
                    drives: p.drives,
                    sets: p.sets,
                    perSet: p.drivesPerSet,
                    parity: p.parity,
                  })}
                </Text>
              </Table.Td>
              <Table.Td>
                {p.current === undefined ? (
                  <Badge color="green" variant="light">
                    {t('clusterExpand.new')}
                  </Badge>
                ) : (
                  <Badge color="gray" variant="light">
                    {p.current === p.index
                      ? t('clusterExpand.kept')
                      : t('clusterExpand.moved', { from: p.current })}
                  </Badge>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
          {plan.removed.map((arg) => (
            <Table.Tr key={arg} style={{ textDecoration: 'line-through' }}>
              <Table.Td>—</Table.Td>
              <Table.Td>
                <Text size="xs" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                  {arg}
                </Text>
              </Table.Td>
              <Table.Td />
              <Table.Td>
                <Badge color="orange" variant="light">
                  {t('clusterExpand.removed')}
                </Badge>
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}

interface ExpandClusterWizardProps {
  opened: boolean;
  onClose: () => void;
  pools: Pools;
}

/**
 * D-CLU-04: the change of the pools of the deployment (S-STO-08), guided.
 * The pools to append (and the decommissioned ones to remove) are checked
 * by the server the way every node parses its command line; the wizard
 * then gives the new NEXO_ENDPOINTS of every node, the checklist of the
 * new nodes and the coordinated stop: the nodes form the new topology once
 * all of them start again with the new command line.
 */
export function ExpandClusterWizard({ opened, onClose, pools }: ExpandClusterWizardProps) {
  const { t } = useTranslation();
  const services = useServices();
  const perms = usePermissions();
  const [step, setStep] = useState(0);
  const [adds, setAdds] = useState<string[]>(['']);
  const [remove, setRemove] = useState<number[]>([]);
  const [checks, setChecks] = useState<Record<string, boolean>>({});
  const [stopped, setStopped] = useState(false);

  const removable = pools.pools.filter((p) => p.decommission?.state === 'complete');
  const plan = useMutation({
    mutationFn: () =>
      services.pools.plan({
        add: adds.map((a) => a.trim()).filter((a) => a !== ''),
        remove,
      }),
    onSuccess: () => {
      setStep(1);
    },
  });
  const stop = useMutation({
    mutationFn: () => services.maintenance.stop(),
    onSuccess: () => {
      setStopped(true);
    },
  });
  const reset = () => {
    setStep(0);
    setAdds(['']);
    setRemove([]);
    setChecks({});
    setStopped(false);
    plan.reset();
    stop.reset();
  };

  const result = plan.data;
  const env = result ? `NEXO_ENDPOINTS="${result.args.join(' ')}"` : '';
  const command = result ? `nexo server ${result.args.map(quote).join(' ')}` : '';
  const checklist: ('prepare' | 'backup' | 'endpoints')[] = [
    ...(result && (result.newNodes.length > 0 || result.pools.some((p) => p.current === undefined))
      ? (['prepare'] as const)
      : []),
    'backup',
    'endpoints',
  ];
  const ready = checklist.every((c) => checks[c] === true);
  const nothing = adds.every((a) => a.trim() === '') && remove.length === 0;

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      onExitTransitionEnd={reset}
      title={t('clusterExpand.title')}
      size="xl"
    >
      <Stepper active={step} size="sm" allowNextStepsSelect={false} onStepClick={setStep}>
        <Stepper.Step label={t('clusterExpand.step1')}>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              plan.mutate();
            }}
          >
            <Stack gap="sm" mt="md">
              <Text size="sm" c="dimmed">
                {t('clusterExpand.addHint')}
              </Text>
              {adds.map((value, i) => (
                <Group key={i} gap="xs" wrap="nowrap" align="flex-end">
                  <TextInput
                    style={{ flex: 1 }}
                    ff="monospace"
                    label={t('clusterExpand.newPool', { index: pools.pools.length + i + 1 })}
                    placeholder="http://nexo{5...8}.lan:9000/mnt/disk{1...4}"
                    value={value}
                    onChange={(e) => {
                      const v = e.currentTarget.value;
                      setAdds((cur) => cur.map((a, j) => (j === i ? v : a)));
                    }}
                  />
                  <ActionIcon
                    variant="default"
                    size="lg"
                    aria-label={t('clusterExpand.removeRow', {
                      index: pools.pools.length + i + 1,
                    })}
                    disabled={adds.length === 1}
                    onClick={() => {
                      setAdds((cur) => cur.filter((_, j) => j !== i));
                    }}
                  >
                    <IconTrash size={16} />
                  </ActionIcon>
                </Group>
              ))}
              <Group>
                <Button
                  size="xs"
                  variant="subtle"
                  leftSection={<IconPlus size={14} />}
                  onClick={() => {
                    setAdds((cur) => [...cur, '']);
                  }}
                >
                  {t('clusterExpand.addRow')}
                </Button>
              </Group>
              {removable.length > 0 ? (
                <Stack gap={4}>
                  <Text size="sm" fw={600}>
                    {t('clusterExpand.removeTitle')}
                  </Text>
                  {removable.map((p) => (
                    <Checkbox
                      key={p.index}
                      label={t('clusterExpand.removePool', { index: p.index, id: p.id })}
                      checked={remove.includes(p.index)}
                      onChange={(e) => {
                        const on = e.currentTarget.checked;
                        setRemove((cur) =>
                          on ? [...cur, p.index] : cur.filter((n) => n !== p.index),
                        );
                      }}
                    />
                  ))}
                </Stack>
              ) : null}
              {plan.isError ? <ErrorAlert error={plan.error} /> : null}
              <Group justify="flex-end" gap="xs">
                <Button variant="default" onClick={onClose}>
                  {t('common.cancel')}
                </Button>
                <Button
                  type="submit"
                  loading={plan.isPending}
                  disabled={nothing || !perms.can(MaintenanceActions.plan)}
                >
                  {t('clusterExpand.check')}
                </Button>
              </Group>
            </Stack>
          </form>
        </Stepper.Step>

        <Stepper.Step label={t('clusterExpand.step2')}>
          {result ? (
            <Stack gap="sm" mt="md">
              <Alert color="green" variant="light" icon={<IconCheck size={18} />}>
                {t('clusterExpand.valid', {
                  pools: result.pools.length,
                  nodes: result.nodes.length,
                })}
              </Alert>
              <PlanTable plan={result} />
              {result.newNodes.length > 0 ? (
                <Text size="sm">
                  {t('clusterExpand.newNodes', { nodes: result.newNodes.join(', ') })}
                </Text>
              ) : null}
              <CommandLine label={t('clusterExpand.env')} value={env} />
              <CommandLine label={t('clusterExpand.command')} value={command} />
              <Group justify="flex-end" gap="xs">
                <Button
                  variant="default"
                  onClick={() => {
                    setStep(0);
                  }}
                >
                  {t('clusterExpand.back')}
                </Button>
                <Button
                  onClick={() => {
                    setStep(2);
                  }}
                >
                  {t('clusterExpand.next')}
                </Button>
              </Group>
            </Stack>
          ) : null}
        </Stepper.Step>

        <Stepper.Step label={t('clusterExpand.step3')}>
          <Stack gap="sm" mt="md">
            {stopped ? (
              <Alert color="orange" variant="light" icon={<IconAlertTriangle size={18} />}>
                {t('clusterExpand.stopped')}
              </Alert>
            ) : (
              <>
                <Text size="sm">{t('clusterExpand.applyHint')}</Text>
                {checklist.map((c) => (
                  <Checkbox
                    key={c}
                    label={t(`clusterExpand.checks.${c}`)}
                    checked={checks[c] === true}
                    onChange={(e) => {
                      const on = e.currentTarget.checked;
                      setChecks((cur) => ({ ...cur, [c]: on }));
                    }}
                  />
                ))}
                <Alert color="red" variant="light" icon={<IconAlertTriangle size={18} />}>
                  {t('clusterExpand.stopHint')}
                </Alert>
                {stop.isError ? <ErrorAlert error={stop.error} /> : null}
                <Group justify="flex-end" gap="xs">
                  <Button
                    variant="default"
                    onClick={() => {
                      setStep(1);
                    }}
                  >
                    {t('clusterExpand.back')}
                  </Button>
                  <Button
                    color="red"
                    disabled={!ready || !perms.can(MaintenanceActions.stop)}
                    loading={stop.isPending}
                    onClick={() => {
                      stop.mutate();
                    }}
                  >
                    {t('clusterExpand.stop')}
                  </Button>
                </Group>
              </>
            )}
          </Stack>
        </Stepper.Step>
      </Stepper>
    </Modal>
  );
}
