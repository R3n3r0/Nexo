import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Group,
  Modal,
  Paper,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconPlus, IconRefresh, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { SiteReplicationActions, usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { clusterRefreshMs } from '@/lib/cluster';
import { problemMessage } from '@/lib/problem';
import { useServices, type SiteReplicationSite } from '@/services';

const siteReplicationKey = ['cluster', 'site-replication'] as const;

const emptySite = (): SiteReplicationSite => ({ name: '', endpoint: '', adminEndpoint: '' });

const urlPattern = /^https?:\/\/[^/\s]+\/?$/;

interface AddSitesProps {
  opened: boolean;
  saving: boolean;
  error: string | undefined;
  onSave: (sites: SiteReplicationSite[]) => void;
  onClose: () => void;
}

/** The sites of a new group: this one first, then the others. */
function AddSites({ opened, saving, error, onSave, onClose }: AddSitesProps) {
  const { t } = useTranslation();
  const [sites, setSites] = useState<SiteReplicationSite[]>([emptySite(), emptySite()]);
  const [submitted, setSubmitted] = useState(false);
  const update = (index: number, change: Partial<SiteReplicationSite>) => {
    setSites((cur) => cur.map((s, i) => (i === index ? { ...s, ...change } : s)));
  };
  const invalid = (s: SiteReplicationSite) => ({
    name: /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/.test(s.name.trim())
      ? undefined
      : t('siteReplication.nameInvalid'),
    endpoint: urlPattern.test(s.endpoint.trim()) ? undefined : t('siteReplication.urlInvalid'),
    adminEndpoint: urlPattern.test(s.adminEndpoint.trim())
      ? undefined
      : t('siteReplication.urlInvalid'),
  });
  const valid = sites.every((s) => Object.values(invalid(s)).every((e) => e === undefined));

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={t('siteReplication.addTitle')}
      centered
      size="xl"
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(true);
          if (!valid) return;
          onSave(
            sites.map((s) => ({
              name: s.name.trim(),
              endpoint: s.endpoint.trim().replace(/\/+$/, ''),
              adminEndpoint: s.adminEndpoint.trim().replace(/\/+$/, ''),
            })),
          );
        }}
      >
        <Stack gap="sm">
          <Text size="sm" c="dimmed">
            {t('siteReplication.addHint')}
          </Text>
          {sites.map((site, index) => {
            const errors = submitted
              ? invalid(site)
              : { name: undefined, endpoint: undefined, adminEndpoint: undefined };
            const label =
              index === 0
                ? t('siteReplication.thisSite')
                : t('siteReplication.otherSite', { index });
            return (
              <Paper
                key={index}
                withBorder
                p="sm"
                radius="sm"
                aria-label={label}
                component="fieldset"
              >
                <Group justify="space-between" mb={4}>
                  <Text size="sm" fw={600}>
                    {label}
                  </Text>
                  {index >= 2 ? (
                    <ActionIcon
                      variant="subtle"
                      color="red"
                      aria-label={t('siteReplication.removeRow', { index })}
                      onClick={() => setSites((cur) => cur.filter((_, i) => i !== index))}
                    >
                      <IconTrash size={16} />
                    </ActionIcon>
                  ) : null}
                </Group>
                <Group grow align="flex-start">
                  <TextInput
                    label={t('siteReplication.name')}
                    value={site.name}
                    onChange={(e) => update(index, { name: e.currentTarget.value })}
                    error={errors.name}
                    withAsterisk
                  />
                  <TextInput
                    label={t('siteReplication.endpoint')}
                    placeholder="https://s3.site-a.example.com"
                    value={site.endpoint}
                    onChange={(e) => update(index, { endpoint: e.currentTarget.value })}
                    error={errors.endpoint}
                    withAsterisk
                  />
                  <TextInput
                    label={t('siteReplication.adminEndpoint')}
                    placeholder="https://console.site-a.example.com"
                    value={site.adminEndpoint}
                    onChange={(e) => update(index, { adminEndpoint: e.currentTarget.value })}
                    error={errors.adminEndpoint}
                    withAsterisk
                  />
                </Group>
              </Paper>
            );
          })}
          <Group>
            <Button
              variant="subtle"
              leftSection={<IconPlus size={16} />}
              onClick={() => setSites((cur) => [...cur, emptySite()])}
            >
              {t('siteReplication.addRow')}
            </Button>
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
              {t('siteReplication.create')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/**
 * Site replication (S-STO-11): the deployments grouped with this one keep
 * their users, groups, access keys, policies and buckets in sync, and
 * every bucket replicates to the same bucket of the other sites.
 */
export function SiteReplication() {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const { siteReplication: service } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const canRead = !perms.ready || perms.can(SiteReplicationActions.info);
  const canAdd = perms.can(SiteReplicationActions.add);
  const canRemove = perms.can(SiteReplicationActions.remove);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState(false);

  const query = useQuery({
    queryKey: siteReplicationKey,
    queryFn: () => service.info(),
    enabled: canRead,
    refetchInterval: clusterRefreshMs,
  });
  const refresh = () => queryClient.invalidateQueries({ queryKey: siteReplicationKey });
  const add = useMutation({
    mutationFn: (sites: SiteReplicationSite[]) => service.add(sites),
    onSuccess: async (info) => {
      queryClient.setQueryData(siteReplicationKey, info);
      setAdding(false);
      notifications.show({ color: 'green', message: t('siteReplication.created') });
      await refresh();
    },
  });
  const remove = useMutation({
    mutationFn: () => service.remove(),
    onSuccess: async () => {
      setRemoving(false);
      notifications.show({ color: 'green', message: t('siteReplication.removed') });
      await refresh();
    },
  });
  const sync = useMutation({
    mutationFn: () => service.sync(),
    onSuccess: async () => {
      notifications.show({ color: 'green', message: t('siteReplication.syncAsked') });
      await refresh();
    },
    onError: (error) => notifications.show({ color: 'red', message: problemMessage(t, error) }),
  });

  if (!canRead) return null;
  const title = t('siteReplication.title');
  const info = query.data;
  const peers = new Map((info?.peers ?? []).map((p) => [p.deploymentId, p]));

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="md">
        <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
          <Stack gap={2}>
            <Title order={4}>{title}</Title>
            <Text size="sm" c="dimmed">
              {t('siteReplication.subtitle')}
            </Text>
          </Stack>
          {info?.enabled ? (
            <Group gap="xs">
              <PermissionGuard allowed={canAdd} action={SiteReplicationActions.add}>
                {(disabled) => (
                  <Button
                    variant="default"
                    leftSection={<IconRefresh size={16} />}
                    disabled={disabled}
                    loading={sync.isPending}
                    onClick={() => sync.mutate()}
                  >
                    {t('siteReplication.sync')}
                  </Button>
                )}
              </PermissionGuard>
              <PermissionGuard allowed={canRemove} action={SiteReplicationActions.remove}>
                {(disabled) => (
                  <Button
                    color="red"
                    variant="light"
                    leftSection={<IconTrash size={16} />}
                    disabled={disabled}
                    onClick={() => {
                      remove.reset();
                      setRemoving(true);
                    }}
                  >
                    {t('siteReplication.remove')}
                  </Button>
                )}
              </PermissionGuard>
            </Group>
          ) : (
            <PermissionGuard allowed={canAdd} action={SiteReplicationActions.add}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled || query.isPending}
                  onClick={() => {
                    add.reset();
                    setAdding(true);
                  }}
                >
                  {t('siteReplication.add')}
                </Button>
              )}
            </PermissionGuard>
          )}
        </Group>
        {query.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, query.error)}
          </Alert>
        ) : null}
        {info && !info.enabled ? (
          <Text size="sm" c="dimmed">
            {t('siteReplication.none')}
          </Text>
        ) : null}
        {info?.enabled ? (
          <>
            <Text size="sm">
              {t('siteReplication.summary', { name: info.name ?? '', count: info.entries })}
            </Text>
            <Table.ScrollContainer minWidth={720}>
              <Table aria-label={title} verticalSpacing="xs">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('siteReplication.name')}</Table.Th>
                    <Table.Th>{t('siteReplication.endpoint')}</Table.Th>
                    <Table.Th>{t('common.status')}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {info.sites.map((s) => {
                    const p = peers.get(s.deploymentId ?? '');
                    let badge = <Badge variant="light">{t('siteReplication.self')}</Badge>;
                    let detail: string | undefined;
                    if (p !== undefined) {
                      if (p.lastError) {
                        badge = (
                          <Badge color="red" variant="light">
                            {t('siteReplication.error')}
                          </Badge>
                        );
                        detail = p.lastError;
                      } else if (p.lastSync) {
                        badge = (
                          <Badge color="green" variant="light">
                            {t('siteReplication.inSync')}
                          </Badge>
                        );
                        detail = t('siteReplication.lastSync', {
                          time: new Date(p.lastSync).toLocaleString(locale),
                        });
                      } else {
                        badge = (
                          <Badge color="gray" variant="light">
                            {t('siteReplication.pending')}
                          </Badge>
                        );
                      }
                    }
                    return (
                      <Table.Tr key={s.name}>
                        <Table.Td>
                          <Text size="sm" fw={600}>
                            {s.name}
                          </Text>
                          <Text size="xs" c="dimmed" ff="monospace">
                            {s.deploymentId}
                          </Text>
                        </Table.Td>
                        <Table.Td>
                          <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                            {s.endpoint}
                          </Text>
                          <Tooltip label={t('siteReplication.adminEndpoint')}>
                            <Text size="xs" c="dimmed" style={{ overflowWrap: 'anywhere' }}>
                              {s.adminEndpoint}
                            </Text>
                          </Tooltip>
                        </Table.Td>
                        <Table.Td>
                          <Stack gap={2}>
                            <Group gap="xs">{badge}</Group>
                            {detail ? (
                              <Text
                                size="xs"
                                c={p?.lastError ? 'red' : 'dimmed'}
                                style={{ overflowWrap: 'anywhere' }}
                              >
                                {detail}
                              </Text>
                            ) : null}
                            {p && p.pending > 0 ? (
                              <Text size="xs" c="orange">
                                {t('siteReplication.pendingChanges', { count: p.pending })}
                              </Text>
                            ) : null}
                          </Stack>
                        </Table.Td>
                      </Table.Tr>
                    );
                  })}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          </>
        ) : null}
      </Stack>
      <AddSites
        opened={adding}
        saving={add.isPending}
        error={add.isError ? problemMessage(t, add.error) : undefined}
        onSave={(sites) => add.mutate(sites)}
        onClose={() => {
          setAdding(false);
          add.reset();
        }}
      />
      <ConfirmDialog
        opened={removing}
        title={t('siteReplication.removeTitle')}
        message={t('siteReplication.removeConfirm')}
        confirmLabel={t('siteReplication.remove')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => setRemoving(false)}
        onConfirm={() => remove.mutate()}
      />
    </Paper>
  );
}
