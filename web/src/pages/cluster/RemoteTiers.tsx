import {
  ActionIcon,
  Alert,
  Button,
  Group,
  Modal,
  Paper,
  PasswordInput,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { TierActions, usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { problemMessage } from '@/lib/problem';
import { useServices, type TierInput } from '@/services';

const tiersKey = ['cluster', 'tiers'] as const;

const namePattern = /^[A-Z0-9_-]{1,64}$/;
const urlPattern = /^https?:\/\/[^/\s]+\/?$/;

const emptyTier = (): TierInput => ({
  name: '',
  endpoint: '',
  bucket: '',
  prefix: '',
  region: '',
  storageClass: '',
  accessKey: '',
  secretKey: '',
});

interface AddTierProps {
  opened: boolean;
  saving: boolean;
  error: string | undefined;
  onSave: (input: TierInput) => void;
  onClose: () => void;
}

/** The form of a new tier. */
function AddTier({ opened, saving, error, onSave, onClose }: AddTierProps) {
  const { t } = useTranslation();
  const [tier, setTier] = useState<TierInput>(emptyTier);
  const [submitted, setSubmitted] = useState(false);
  const set = (change: Partial<TierInput>) => setTier((cur) => ({ ...cur, ...change }));
  const errors = {
    name:
      namePattern.test(tier.name ?? '') &&
      tier.name !== 'STANDARD' &&
      tier.name !== 'REDUCED_REDUNDANCY'
        ? undefined
        : t('remoteTiers.nameInvalid'),
    endpoint: urlPattern.test((tier.endpoint ?? '').trim())
      ? undefined
      : t('remoteTiers.urlInvalid'),
    bucket: (tier.bucket ?? '').trim() ? undefined : t('remoteTiers.required'),
    prefix: !tier.prefix || tier.prefix.endsWith('/') ? undefined : t('remoteTiers.prefixInvalid'),
    accessKey: tier.accessKey.trim() ? undefined : t('remoteTiers.required'),
    secretKey: tier.secretKey ? undefined : t('remoteTiers.required'),
  };
  const valid = Object.values(errors).every((e) => e === undefined);
  const shown = (e: string | undefined) => (submitted ? e : undefined);
  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={t('remoteTiers.addTitle')}
      size="lg"
      onExitTransitionEnd={() => {
        setTier(emptyTier());
        setSubmitted(false);
      }}
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(true);
          if (!valid) return;
          onSave({
            ...tier,
            endpoint: (tier.endpoint ?? '').trim().replace(/\/+$/, ''),
            bucket: (tier.bucket ?? '').trim(),
            prefix: tier.prefix || undefined,
            region: tier.region || undefined,
            storageClass: tier.storageClass || undefined,
          });
        }}
      >
        <Stack gap="sm">
          <Text size="sm" c="dimmed">
            {t('remoteTiers.addHint')}
          </Text>
          <TextInput
            label={t('remoteTiers.name')}
            description={t('remoteTiers.nameHint')}
            withAsterisk
            value={tier.name}
            error={shown(errors.name)}
            onChange={(e) => set({ name: e.currentTarget.value.toUpperCase() })}
          />
          <TextInput
            label={t('remoteTiers.endpoint')}
            placeholder="https://s3.example.com"
            withAsterisk
            value={tier.endpoint}
            error={shown(errors.endpoint)}
            onChange={(e) => set({ endpoint: e.currentTarget.value })}
          />
          <Group grow align="flex-start">
            <TextInput
              label={t('remoteTiers.bucket')}
              withAsterisk
              value={tier.bucket}
              error={shown(errors.bucket)}
              onChange={(e) => set({ bucket: e.currentTarget.value })}
            />
            <TextInput
              label={t('remoteTiers.prefix')}
              placeholder="nexo/"
              value={tier.prefix}
              error={shown(errors.prefix)}
              onChange={(e) => set({ prefix: e.currentTarget.value })}
            />
          </Group>
          <Group grow align="flex-start">
            <TextInput
              label={t('remoteTiers.region')}
              placeholder="us-east-1"
              value={tier.region}
              onChange={(e) => set({ region: e.currentTarget.value })}
            />
            <TextInput
              label={t('remoteTiers.storageClass')}
              value={tier.storageClass}
              onChange={(e) => set({ storageClass: e.currentTarget.value })}
            />
          </Group>
          <Group grow align="flex-start">
            <TextInput
              label={t('remoteTiers.accessKey')}
              withAsterisk
              autoComplete="off"
              value={tier.accessKey}
              error={shown(errors.accessKey)}
              onChange={(e) => set({ accessKey: e.currentTarget.value })}
            />
            <PasswordInput
              label={t('remoteTiers.secretKey')}
              withAsterisk
              autoComplete="new-password"
              value={tier.secretKey}
              error={shown(errors.secretKey)}
              onChange={(e) => set({ secretKey: e.currentTarget.value })}
            />
          </Group>
          {error ? (
            <Alert color="red" variant="light" role="alert">
              {error}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={saving}>
              {t('remoteTiers.create')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/**
 * Remote tiers (S-STO-12): the S3 buckets the Transition actions of the
 * bucket lifecycle move the data of old versions to.
 */
export function RemoteTiers() {
  const { t } = useTranslation();
  const { tiers: service } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const canRead = !perms.ready || perms.can(TierActions.list);
  const canSet = perms.can(TierActions.set);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<string | null>(null);

  const query = useQuery({ queryKey: tiersKey, queryFn: () => service.list(), enabled: canRead });
  const refresh = () => queryClient.invalidateQueries({ queryKey: tiersKey });
  const add = useMutation({
    mutationFn: (input: TierInput) => service.add(input),
    onSuccess: async (tier) => {
      setAdding(false);
      notifications.show({
        color: 'green',
        message: t('remoteTiers.created', { name: tier.name }),
      });
      await refresh();
    },
  });
  const remove = useMutation({
    mutationFn: (name: string) => service.remove(name),
    onSuccess: async (_, name) => {
      setRemoving(null);
      notifications.show({ color: 'green', message: t('remoteTiers.removed', { name }) });
      await refresh();
    },
  });

  if (!canRead) return null;
  const title = t('remoteTiers.title');
  const tiers = query.data ?? [];
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="md">
        <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
          <Stack gap={2}>
            <Title order={4}>{title}</Title>
            <Text size="sm" c="dimmed">
              {t('remoteTiers.subtitle')}
            </Text>
          </Stack>
          <PermissionGuard allowed={canSet} action={TierActions.set}>
            {(disabled) => (
              <Button
                leftSection={<IconPlus size={16} />}
                disabled={disabled || query.isPending}
                onClick={() => {
                  add.reset();
                  setAdding(true);
                }}
              >
                {t('remoteTiers.add')}
              </Button>
            )}
          </PermissionGuard>
        </Group>
        {query.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, query.error)}
          </Alert>
        ) : null}
        {query.isSuccess && tiers.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('remoteTiers.none')}
          </Text>
        ) : null}
        {tiers.length > 0 ? (
          <Table.ScrollContainer minWidth={640}>
            <Table aria-label={title} verticalSpacing="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('remoteTiers.name')}</Table.Th>
                  <Table.Th>{t('remoteTiers.endpoint')}</Table.Th>
                  <Table.Th>{t('remoteTiers.bucket')}</Table.Th>
                  <Table.Th>{t('remoteTiers.accessKey')}</Table.Th>
                  <Table.Th aria-label={t('common.actions')} />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {tiers.map((tier) => (
                  <Table.Tr key={tier.name}>
                    <Table.Td>
                      <Text size="sm" fw={600} ff="monospace">
                        {tier.name}
                      </Text>
                      {tier.storageClass ? (
                        <Text size="xs" c="dimmed">
                          {tier.storageClass}
                        </Text>
                      ) : null}
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                        {tier.endpoint}
                      </Text>
                      {tier.region ? (
                        <Text size="xs" c="dimmed">
                          {tier.region}
                        </Text>
                      ) : null}
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                        {tier.bucket}
                        {tier.prefix ? `/${tier.prefix.replace(/\/$/, '')}` : ''}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" ff="monospace">
                        {tier.accessKey}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <PermissionGuard allowed={canSet} action={TierActions.set}>
                        {(disabled) => (
                          <Tooltip label={t('remoteTiers.removeTier', { name: tier.name })}>
                            <ActionIcon
                              variant="subtle"
                              color="red"
                              aria-label={t('remoteTiers.removeTier', { name: tier.name })}
                              disabled={disabled}
                              onClick={() => {
                                remove.reset();
                                setRemoving(tier.name);
                              }}
                            >
                              <IconTrash size={16} />
                            </ActionIcon>
                          </Tooltip>
                        )}
                      </PermissionGuard>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        ) : null}
      </Stack>
      <AddTier
        opened={adding}
        saving={add.isPending}
        error={add.isError ? problemMessage(t, add.error) : undefined}
        onSave={(input) => add.mutate(input)}
        onClose={() => {
          setAdding(false);
          add.reset();
        }}
      />
      <ConfirmDialog
        opened={removing !== null}
        title={t('remoteTiers.removeTitle')}
        message={t('remoteTiers.removeConfirm', { name: removing ?? '' })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => setRemoving(null)}
        onConfirm={() => removing !== null && remove.mutate(removing)}
      />
    </Paper>
  );
}
