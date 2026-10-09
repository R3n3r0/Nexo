import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Modal,
  Paper,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  Switch,
  Table,
  Text,
  Textarea,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconKey, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { DistributionActions, usePermissions } from '@/auth/usePermissions';
import { RevealOnceModal } from '@/components/common/RevealOnceModal';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { distributionKeys } from '@/lib/accessQueries';
import { isValidDistributionId } from '@/lib/distributions';
import { problemMessage } from '@/lib/problem';
import {
  useServices,
  type AddKeyInput,
  type CreateKeyGroupInput,
  type KeyGroup,
  type PublicKey,
} from '@/services';

import { DistributionsHeader } from './DistributionsHeader';

/** A private key just generated, shown once. */
interface Revealed {
  group: string;
  key: PublicKey;
  privateKeyPem: string;
}

type Algorithm = 'ed25519' | 'rsa';

/** The key generation fields shared by the creation and the new key dialogs. */
function KeyFields({
  algorithm,
  bits,
  onAlgorithm,
  onBits,
}: {
  algorithm: Algorithm;
  bits: string;
  onAlgorithm: (a: Algorithm) => void;
  onBits: (b: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <Group gap="sm" align="flex-end">
      <Stack gap={4}>
        <Text size="sm" fw={500} id="key-algorithm">
          {t('keyGroups.algorithm')}
        </Text>
        <SegmentedControl
          aria-labelledby="key-algorithm"
          value={algorithm}
          data={[
            { value: 'ed25519', label: 'Ed25519' },
            { value: 'rsa', label: 'RSA' },
          ]}
          onChange={(value) => {
            onAlgorithm(value === 'rsa' ? 'rsa' : 'ed25519');
          }}
        />
      </Stack>
      {algorithm === 'rsa' ? (
        <Select
          label={t('keyGroups.bits')}
          data={['2048', '3072', '4096']}
          value={bits}
          allowDeselect={false}
          w={120}
          onChange={(value) => {
            if (value !== null) onBits(value);
          }}
        />
      ) : null}
    </Group>
  );
}

function CreateDialog({
  opened,
  onClose,
  onCreated,
}: {
  opened: boolean;
  onClose: () => void;
  onCreated: (revealed: Revealed | null) => void;
}) {
  const { t } = useTranslation();
  const { keyGroups } = useServices();
  const queryClient = useQueryClient();
  const [id, setId] = useState('');
  const [comment, setComment] = useState('');
  const [generate, setGenerate] = useState(true);
  const [algorithm, setAlgorithm] = useState<Algorithm>('ed25519');
  const [bits, setBits] = useState('2048');
  const [touched, setTouched] = useState(false);
  const create = useMutation({
    mutationFn: (input: CreateKeyGroupInput) => keyGroups.create(input),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: distributionKeys.keyGroups });
      setId('');
      setComment('');
      onCreated(
        result.key !== undefined
          ? {
              group: result.keyGroup.id,
              key: result.key.publicKey,
              privateKeyPem: result.key.privateKeyPem,
            }
          : null,
      );
    },
  });
  const invalid = !isValidDistributionId(id);
  return (
    <Modal opened={opened} onClose={onClose} title={t('keyGroups.createTitle')}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          setTouched(true);
          if (invalid) return;
          create.mutate({
            id,
            ...(comment.trim() !== '' ? { comment: comment.trim() } : {}),
            ...(generate
              ? {
                  generateKey: {
                    algorithm,
                    ...(algorithm === 'rsa' ? { bits: Number(bits) } : {}),
                  },
                }
              : {}),
          });
        }}
      >
        <Stack gap="sm">
          <TextInput
            label={t('keyGroups.id')}
            description={t('keyGroups.idHint')}
            required
            value={id}
            error={touched && invalid ? t('keyGroups.idInvalid') : undefined}
            onChange={(event) => {
              setId(event.currentTarget.value);
            }}
          />
          <TextInput
            label={t('keyGroups.comment')}
            value={comment}
            onChange={(event) => {
              setComment(event.currentTarget.value);
            }}
          />
          <Switch
            label={t('keyGroups.generate')}
            description={t('keyGroups.generateHint')}
            checked={generate}
            onChange={(event) => {
              setGenerate(event.currentTarget.checked);
            }}
          />
          {generate ? (
            <KeyFields
              algorithm={algorithm}
              bits={bits}
              onAlgorithm={setAlgorithm}
              onBits={setBits}
            />
          ) : null}
          {create.isError ? <ErrorAlert error={create.error} /> : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={create.isPending}>
              {t('common.create')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function AddKeyDialog({
  group,
  onClose,
  onAdded,
}: {
  group: string | null;
  onClose: () => void;
  onAdded: (revealed: Revealed | null) => void;
}) {
  const { t } = useTranslation();
  const { keyGroups } = useServices();
  const queryClient = useQueryClient();
  const [source, setSource] = useState<'generate' | 'upload'>('generate');
  const [algorithm, setAlgorithm] = useState<Algorithm>('ed25519');
  const [bits, setBits] = useState('2048');
  const [pem, setPem] = useState('');
  const [comment, setComment] = useState('');
  const add = useMutation({
    mutationFn: (input: AddKeyInput) => keyGroups.addKey(group ?? '', input),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: distributionKeys.keyGroups });
      setPem('');
      setComment('');
      onAdded(
        result.privateKeyPem !== undefined
          ? { group: group ?? '', key: result.publicKey, privateKeyPem: result.privateKeyPem }
          : null,
      );
    },
  });
  return (
    <Modal
      opened={group !== null}
      onClose={onClose}
      title={t('keyGroups.addKeyTitle', { group: group ?? '' })}
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          add.mutate({
            ...(comment.trim() !== '' ? { comment: comment.trim() } : {}),
            ...(source === 'upload'
              ? { publicKeyPem: pem }
              : { algorithm, ...(algorithm === 'rsa' ? { bits: Number(bits) } : {}) }),
          });
        }}
      >
        <Stack gap="sm">
          <SegmentedControl
            aria-label={t('keyGroups.keySource')}
            value={source}
            data={[
              { value: 'generate', label: t('keyGroups.sources.generate') },
              { value: 'upload', label: t('keyGroups.sources.upload') },
            ]}
            onChange={(value) => {
              setSource(value === 'upload' ? 'upload' : 'generate');
            }}
          />
          {source === 'generate' ? (
            <KeyFields
              algorithm={algorithm}
              bits={bits}
              onAlgorithm={setAlgorithm}
              onBits={setBits}
            />
          ) : (
            <Textarea
              label={t('keyGroups.publicKey')}
              description={t('keyGroups.publicKeyHint')}
              placeholder="-----BEGIN PUBLIC KEY-----"
              autosize
              minRows={4}
              required
              value={pem}
              styles={{
                input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 },
              }}
              onChange={(event) => {
                setPem(event.currentTarget.value);
              }}
            />
          )}
          <TextInput
            label={t('keyGroups.comment')}
            value={comment}
            onChange={(event) => {
              setComment(event.currentTarget.value);
            }}
          />
          {add.isError ? <ErrorAlert error={add.error} /> : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button
              type="submit"
              loading={add.isPending}
              disabled={source === 'upload' && pem.trim() === ''}
            >
              {t('keyGroups.addKey')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function GroupCard({
  group,
  canManage,
  onAddKey,
  onRemoveKey,
  onDelete,
}: {
  group: KeyGroup;
  canManage: boolean;
  onAddKey: () => void;
  onRemoveKey: (key: PublicKey) => void;
  onDelete: () => void;
}) {
  const { t, i18n } = useTranslation();
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('keyGroups.groupLabel', { id: group.id })}
    >
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Stack gap={0}>
            <Group gap="xs">
              <IconKey size={18} aria-hidden="true" />
              <Title order={4}>{group.id}</Title>
              <Badge variant="light">{t('keyGroups.keyCount', { count: group.keys.length })}</Badge>
            </Group>
            {group.comment !== undefined ? (
              <Text size="sm" c="dimmed">
                {group.comment}
              </Text>
            ) : null}
          </Stack>
          {canManage ? (
            <Group gap="xs">
              <Button
                size="xs"
                variant="default"
                leftSection={<IconPlus size={14} />}
                onClick={onAddKey}
              >
                {t('keyGroups.addKey')}
              </Button>
              <Button
                size="xs"
                color="red"
                variant="subtle"
                leftSection={<IconTrash size={14} />}
                onClick={onDelete}
              >
                {t('keyGroups.delete')}
              </Button>
            </Group>
          ) : null}
        </Group>
        {group.keys.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('keyGroups.noKeys')}
          </Text>
        ) : (
          <Table aria-label={t('keyGroups.keysLabel', { id: group.id })} verticalSpacing={4}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('keyGroups.keyPairId')}</Table.Th>
                <Table.Th>{t('keyGroups.algorithm')}</Table.Th>
                <Table.Th>{t('common.created')}</Table.Th>
                <Table.Th>{t('keyGroups.comment')}</Table.Th>
                <Table.Th w={48} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {group.keys.map((k) => (
                <Table.Tr key={k.id}>
                  <Table.Td>
                    <Text size="sm" ff="monospace">
                      {k.id}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    {k.algorithm === 'rsa' ? `RSA ${String(k.bits ?? '')}` : 'Ed25519'}
                  </Table.Td>
                  <Table.Td>{new Date(k.createdAt).toLocaleString(i18n.resolvedLanguage)}</Table.Td>
                  <Table.Td>{k.comment ?? ''}</Table.Td>
                  <Table.Td>
                    {canManage ? (
                      <Tooltip label={t('keyGroups.removeKey', { id: k.id })}>
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          aria-label={t('keyGroups.removeKey', { id: k.id })}
                          onClick={() => {
                            onRemoveKey(k);
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
        )}
      </Stack>
    </Paper>
  );
}

/**
 * D-DST-05: the key groups of the signed behaviors. A generated private
 * key is shown once (copy, download) and never stored; several keys sign
 * at once, so a key is rotated by adding the new one before removing the
 * old one.
 */
export function KeyGroupsPage() {
  const { t } = useTranslation();
  const { keyGroups } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const canManage = perms.can(DistributionActions.manage);
  const list = useQuery({ queryKey: distributionKeys.keyGroups, queryFn: () => keyGroups.list() });
  const [creating, setCreating] = useState(false);
  const [adding, setAdding] = useState<string | null>(null);
  const [revealed, setRevealed] = useState<Revealed | null>(null);
  const [removing, setRemoving] = useState<{ group: string; key?: string } | null>(null);
  const remove = useMutation({
    mutationFn: async (target: { group: string; key?: string }) => {
      if (target.key !== undefined) await keyGroups.removeKey(target.group, target.key);
      else await keyGroups.remove(target.group);
    },
    onSuccess: async () => {
      setRemoving(null);
      await queryClient.invalidateQueries({ queryKey: distributionKeys.keyGroups });
      notifications.show({ color: 'green', message: t('keyGroups.removed') });
    },
  });

  return (
    <Stack gap="md">
      <DistributionsHeader
        active="key-groups"
        actions={
          <PermissionGuard allowed={canManage} action={DistributionActions.manage}>
            {(disabled) => (
              <Button
                leftSection={<IconPlus size={16} />}
                disabled={disabled}
                onClick={() => {
                  setCreating(true);
                }}
              >
                {t('keyGroups.create')}
              </Button>
            )}
          </PermissionGuard>
        }
      />
      <Text size="sm" c="dimmed">
        {t('keyGroups.description')}
      </Text>
      {list.isError ? (
        <ErrorAlert error={list.error} title={t('keyGroups.loadError')} />
      ) : list.isPending ? (
        <Skeleton height={120} />
      ) : list.data.length === 0 ? (
        <Paper withBorder p="xl" radius="md">
          <Text c="dimmed" ta="center">
            {t('keyGroups.empty')}
          </Text>
        </Paper>
      ) : (
        list.data.map((g) => (
          <GroupCard
            key={g.id}
            group={g}
            canManage={canManage}
            onAddKey={() => {
              setAdding(g.id);
            }}
            onRemoveKey={(k) => {
              remove.reset();
              setRemoving({ group: g.id, key: k.id });
            }}
            onDelete={() => {
              remove.reset();
              setRemoving({ group: g.id });
            }}
          />
        ))
      )}
      <CreateDialog
        opened={creating}
        onClose={() => {
          setCreating(false);
        }}
        onCreated={(r) => {
          setCreating(false);
          setRevealed(r);
        }}
      />
      <AddKeyDialog
        group={adding}
        onClose={() => {
          setAdding(null);
        }}
        onAdded={(r) => {
          setAdding(null);
          setRevealed(r);
        }}
      />
      <RevealOnceModal
        title={t('keyGroups.privateKeyTitle')}
        description={
          revealed !== null
            ? t('keyGroups.privateKeyHint', { group: revealed.group, key: revealed.key.id })
            : undefined
        }
        secrets={
          revealed === null
            ? null
            : [
                { label: t('keyGroups.keyPairId'), value: revealed.key.id },
                { label: t('keyGroups.privateKey'), value: revealed.privateKeyPem },
              ]
        }
        download={
          revealed === null
            ? undefined
            : {
                filename: `${revealed.group}-${revealed.key.id}.pem`,
                text: revealed.privateKeyPem,
                type: 'application/x-pem-file',
              }
        }
        onClose={() => {
          setRevealed(null);
        }}
      />
      <ConfirmDialog
        opened={removing !== null}
        title={
          removing?.key !== undefined ? t('keyGroups.removeKeyTitle') : t('keyGroups.deleteTitle')
        }
        message={
          removing?.key !== undefined
            ? t('keyGroups.removeKeyConfirm', { key: removing.key, group: removing.group })
            : t('keyGroups.deleteConfirm', { group: removing?.group ?? '' })
        }
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setRemoving(null);
        }}
        onConfirm={() => {
          if (removing !== null) remove.mutate(removing);
        }}
      />
      {remove.isError && removing === null ? (
        <Alert color="red" variant="light">
          {problemMessage(t, remove.error)}
        </Alert>
      ) : null}
    </Stack>
  );
}
