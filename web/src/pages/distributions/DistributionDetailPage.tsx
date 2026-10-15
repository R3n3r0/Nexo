import {
  Alert,
  Anchor,
  Badge,
  Button,
  Group,
  Paper,
  SegmentedControl,
  Select,
  SimpleGrid,
  Skeleton,
  Stack,
  Switch,
  Tabs,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconAlertTriangle, IconDeviceFloppy, IconRefresh, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router';

import { DistributionActions, usePermissions } from '@/auth/usePermissions';
import { AliasesField } from '@/components/distributions/AliasesField';
import { BehaviorsPanel } from '@/components/distributions/BehaviorsPanel';
import { DnsInstructions } from '@/components/distributions/DnsInstructions';
import { DistributionDocument } from '@/components/distributions/DistributionDocument';
import { ResponsesPanel } from '@/components/distributions/ResponsesPanel';
import { SecurityPanel } from '@/components/distributions/SecurityPanel';
import { SignedUrlGenerator } from '@/components/distributions/SignedUrlGenerator';
import { TlsPanel } from '@/components/distributions/TlsPanel';
import { TrafficTable } from '@/components/distributions/TrafficTable';
import { RevealOnceModal } from '@/components/common/RevealOnceModal';
import { ConfirmDialog, ErrorAlert } from '@/components/iam/common';
import { distributionKeys } from '@/lib/accessQueries';
import { editable, signingKeyGroups } from '@/lib/distributions';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import {
  isApiError,
  useServices,
  type Distribution,
  type RotateOriginSecretInput,
  type RotateOriginSecretResult,
} from '@/services';

import { DistributionCacheTab } from './DistributionCacheTab';
import { DistributionsHeader } from './DistributionsHeader';

const tabs = [
  'overview',
  'behaviors',
  'responses',
  'security',
  'tls',
  'cache',
  'document',
] as const;
type DetailTab = (typeof tabs)[number];

function same(a: Distribution, b: Distribution): boolean {
  return JSON.stringify(editable(a)) === JSON.stringify(editable(b));
}

/**
 * D-DST-01..08: a distribution. Every tab edits a draft of the whole
 * document, saved at once with If-Match on its etag (a concurrent change
 * answers 412 and is reported); the origin secret rotation and the signed
 * URLs are immediate server actions.
 */
export function DistributionDetailPage() {
  const { t } = useTranslation();
  const { id = '', tab = 'overview' } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { distributions, keyGroups, buckets } = useServices();
  const perms = usePermissions();
  const canManage = perms.can(DistributionActions.manage);
  const canSign = perms.can(DistributionActions.sign);
  const canCerts = perms.can(DistributionActions.certificates);
  const active: DetailTab = (tabs as readonly string[]).includes(tab)
    ? (tab as DetailTab)
    : 'overview';
  const key = distributionKeys.one(id);
  const query = useQuery({ queryKey: key, queryFn: () => distributions.get(id) });
  const groups = useQuery({
    queryKey: distributionKeys.keyGroups,
    queryFn: () => keyGroups.list(),
  });
  const bucketList = useQuery({ queryKey: storageKeys.buckets, queryFn: () => buckets.list() });
  const traffic = useQuery({
    queryKey: distributionKeys.traffic(id),
    queryFn: () => distributions.traffic(id),
    retry: false,
  });
  const [draft, setDraft] = useState<Distribution | null>(null);
  const [revealed, setRevealed] = useState<RotateOriginSecretResult | null>(null);
  const [deleting, setDeleting] = useState(false);

  const base = query.data;
  const current = draft ?? base;
  const dirty = draft !== null && base !== undefined && !same(draft, base);
  const readOnly = !canManage;

  const save = useMutation({
    mutationFn: (d: Distribution) => distributions.update(editable(d), base?.etag),
    onSuccess: async (stored) => {
      queryClient.setQueryData(key, stored);
      setDraft(null);
      await queryClient.invalidateQueries({ queryKey: distributionKeys.list });
      notifications.show({ color: 'green', message: t('distributions.saved') });
    },
  });
  const rotate = useMutation({
    mutationFn: (input: RotateOriginSecretInput) => distributions.rotateOriginSecret(id, input),
    onSuccess: (result) => {
      queryClient.setQueryData(key, result.distribution);
      setDraft(null);
      setRevealed(result);
    },
  });
  const remove = useMutation({
    mutationFn: () => distributions.remove(id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: distributionKeys.all });
      notifications.show({ color: 'green', message: t('distributions.deleted', { id }) });
      void navigate('/distributions', { replace: true });
    },
  });
  const conflict = isApiError(save.error) && save.error.isPreconditionFailed;

  const change = (next: Distribution) => {
    save.reset();
    setDraft(next);
  };

  if (query.isError) {
    return (
      <Stack gap="md">
        <DistributionsHeader active="list" />
        <ErrorAlert
          error={query.error}
          title={
            isApiError(query.error) && query.error.isNotFound
              ? t('distributions.notFound', { id })
              : t('distributions.loadError')
          }
        />
        <Anchor component={Link} to="/distributions">
          {t('wizard.backToList')}
        </Anchor>
      </Stack>
    );
  }
  if (current === undefined) {
    return (
      <Stack gap="md">
        <DistributionsHeader active="list" />
        <Skeleton height={240} />
      </Stack>
    );
  }

  const keyGroupIds = (groups.data ?? []).map((g) => g.id);
  const signedGroups = signingKeyGroups(current);
  const keyPairs = (groups.data ?? [])
    .filter((g) => signedGroups.includes(g.id))
    .flatMap((g) => g.keys.map((k) => ({ group: g.id, id: k.id })));

  return (
    <Stack gap="md">
      <DistributionsHeader active="list" />
      <Group justify="space-between" align="flex-start" wrap="wrap">
        <Stack gap={2}>
          <Anchor component={Link} to="/distributions" size="sm">
            {t('wizard.backToList')}
          </Anchor>
          <Group gap="xs">
            <Title order={2}>{current.id}</Title>
            <Badge variant="light">{t(`distributions.modes.${current.mode ?? 'website'}`)}</Badge>
            <Badge color={current.enabled === false ? 'gray' : 'teal'} variant="light">
              {current.enabled === false ? t('distributions.disabled') : t('distributions.enabled')}
            </Badge>
          </Group>
          <Text size="sm" c="dimmed" ff="monospace">
            {current.origin.bucket}
            {current.origin.pathPrefix ?? ''}
          </Text>
        </Stack>
        {canManage ? (
          <Button
            color="red"
            variant="light"
            leftSection={<IconTrash size={16} />}
            onClick={() => {
              setDeleting(true);
            }}
          >
            {t('distributions.delete')}
          </Button>
        ) : null}
      </Group>

      {dirty || conflict || save.isError ? (
        <Paper
          withBorder
          radius="md"
          p="sm"
          role="region"
          aria-label={t('distributions.unsavedTitle')}
          style={{
            position: 'sticky',
            top: 64,
            zIndex: 5,
            background: 'var(--mantine-color-body)',
          }}
        >
          <Stack gap="xs">
            {conflict ? (
              <Alert
                color="yellow"
                variant="light"
                icon={<IconAlertTriangle size={18} />}
                title={t('distributions.conflictTitle')}
              >
                {t('distributions.conflict')}
              </Alert>
            ) : save.isError ? (
              <Alert color="red" variant="light" role="alert">
                {problemMessage(t, save.error)}
              </Alert>
            ) : null}
            <Group justify="space-between" wrap="wrap">
              <Text size="sm" fw={500}>
                {dirty ? t('distributions.unsaved') : ''}
              </Text>
              <Group gap="xs">
                <Button
                  variant="default"
                  leftSection={conflict ? <IconRefresh size={16} /> : undefined}
                  onClick={() => {
                    setDraft(null);
                    save.reset();
                    if (conflict) void query.refetch();
                  }}
                >
                  {conflict ? t('distributions.reload') : t('distributions.discard')}
                </Button>
                {!conflict ? (
                  <Button
                    leftSection={<IconDeviceFloppy size={16} />}
                    loading={save.isPending}
                    disabled={!dirty}
                    onClick={() => {
                      if (draft !== null) save.mutate(draft);
                    }}
                  >
                    {t('distributions.save')}
                  </Button>
                ) : null}
              </Group>
            </Group>
          </Stack>
        </Paper>
      ) : null}

      <Tabs
        value={active}
        keepMounted={false}
        onChange={(value) => {
          if (value !== null) void navigate(`/distributions/${encodeURIComponent(id)}/${value}`);
        }}
      >
        <Tabs.List>
          {tabs.map((value) => (
            <Tabs.Tab key={value} value={value}>
              {t(`distributions.detailTabs.${value}`)}
            </Tabs.Tab>
          ))}
        </Tabs.List>
        {tabs.map((value) => (
          <Tabs.Panel key={value} value={value} pt="md">
            {value !== active ? null : value === 'overview' ? (
              <Stack gap="md">
                <Paper
                  withBorder
                  radius="md"
                  p="md"
                  component="section"
                  aria-label={t('distributions.general')}
                >
                  <Stack gap="sm">
                    <Title order={4}>{t('distributions.general')}</Title>
                    <Switch
                      label={t('distributions.fields.enabled')}
                      description={t('distributions.enabledHint')}
                      checked={current.enabled !== false}
                      disabled={readOnly}
                      onChange={(event) => {
                        change({ ...current, enabled: event.currentTarget.checked });
                      }}
                    />
                    <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                      <Select
                        label={t('distributions.fields.bucket')}
                        searchable
                        data={(bucketList.data ?? [])
                          .map((b) => b.name)
                          .concat(
                            (bucketList.data ?? []).some((b) => b.name === current.origin.bucket)
                              ? []
                              : [current.origin.bucket],
                          )}
                        value={current.origin.bucket}
                        allowDeselect={false}
                        disabled={readOnly}
                        onChange={(bucket) => {
                          if (bucket !== null)
                            change({ ...current, origin: { ...current.origin, bucket } });
                        }}
                      />
                      <TextInput
                        label={t('distributions.fields.pathPrefix')}
                        value={current.origin.pathPrefix ?? ''}
                        disabled={readOnly || current.mode === 's3'}
                        onChange={(event) => {
                          change({
                            ...current,
                            origin: { ...current.origin, pathPrefix: event.currentTarget.value },
                          });
                        }}
                      />
                      <TextInput
                        label={t('distributions.fields.comment')}
                        value={current.comment ?? ''}
                        disabled={readOnly}
                        onChange={(event) => {
                          change({ ...current, comment: event.currentTarget.value });
                        }}
                      />
                      <Stack gap={4}>
                        <Text size="sm" fw={500} id="distribution-mode">
                          {t('distributions.fields.mode')}
                        </Text>
                        <SegmentedControl
                          aria-labelledby="distribution-mode"
                          value={current.mode ?? 'website'}
                          disabled={readOnly}
                          data={[
                            { value: 'website', label: t('distributions.modes.website') },
                            { value: 's3', label: t('distributions.modes.s3') },
                          ]}
                          onChange={(mode) => {
                            change({ ...current, mode: mode === 's3' ? 's3' : 'website' });
                          }}
                          style={{ alignSelf: 'flex-start' }}
                        />
                      </Stack>
                    </SimpleGrid>
                  </Stack>
                </Paper>
                <Paper
                  withBorder
                  radius="md"
                  p="md"
                  component="section"
                  aria-label={t('distributions.domains')}
                >
                  <Stack gap="sm">
                    <Title order={4}>{t('distributions.domains')}</Title>
                    <AliasesField
                      value={current.aliases}
                      distribution={current.id}
                      disabled={readOnly}
                      onChange={(aliases) => {
                        change({ ...current, aliases });
                      }}
                    />
                    <Title order={5}>{t('dns.title')}</Title>
                    <DnsInstructions aliases={base?.aliases ?? []} distribution={current.id} />
                  </Stack>
                </Paper>
                <Paper
                  withBorder
                  radius="md"
                  p="md"
                  component="section"
                  aria-label={t('distributions.traffic.title')}
                >
                  <Stack gap="sm">
                    <Title order={4}>{t('distributions.traffic.title')}</Title>
                    {traffic.isPending ? (
                      <Skeleton height={60} />
                    ) : traffic.data === null || traffic.isError ? (
                      <Text size="sm" c="dimmed">
                        {t('distributions.traffic.unavailable')}
                      </Text>
                    ) : (
                      <TrafficTable hosts={traffic.data.hosts} />
                    )}
                  </Stack>
                </Paper>
              </Stack>
            ) : value === 'behaviors' ? (
              <BehaviorsPanel
                draft={current}
                onChange={change}
                dirty={dirty}
                keyGroups={keyGroupIds}
                readOnly={readOnly}
              />
            ) : value === 'responses' ? (
              <ResponsesPanel draft={current} onChange={change} readOnly={readOnly} />
            ) : value === 'security' ? (
              <Stack gap="md">
                <SecurityPanel
                  draft={current}
                  onChange={change}
                  readOnly={readOnly}
                  dirty={dirty}
                  rotating={rotate.isPending}
                  onRotate={(input) => {
                    rotate.mutate(input);
                  }}
                />
                {rotate.isError ? (
                  <Alert color="red" variant="light" role="alert">
                    {problemMessage(t, rotate.error)}
                  </Alert>
                ) : null}
                {signedGroups.length > 0 ? (
                  canSign ? (
                    <SignedUrlGenerator distribution={current} keyPairs={keyPairs} />
                  ) : (
                    <Text size="sm" c="dimmed">
                      {t('permissions.missing', { action: DistributionActions.sign })}
                    </Text>
                  )
                ) : (
                  <Text size="sm" c="dimmed">
                    {t('signing.noSignedBehavior')}
                  </Text>
                )}
              </Stack>
            ) : value === 'cache' ? (
              <DistributionCacheTab distribution={base ?? current} />
            ) : value === 'tls' ? (
              <TlsPanel
                draft={current}
                onChange={change}
                readOnly={readOnly}
                canReadCertificates={canCerts}
              />
            ) : (
              <DistributionDocument
                key={base?.etag ?? ''}
                distribution={current}
                readOnly={readOnly}
                onApply={change}
              />
            )}
          </Tabs.Panel>
        ))}
      </Tabs>

      <RevealOnceModal
        title={t('distributions.security.secretTitle')}
        description={
          revealed !== null
            ? t('distributions.security.revealHint', { header: revealed.header })
            : undefined
        }
        secrets={revealed === null ? null : [{ label: revealed.header, value: revealed.secret }]}
        onClose={() => {
          setRevealed(null);
        }}
      />
      <ConfirmDialog
        opened={deleting}
        title={t('distributions.deleteTitle')}
        message={t('distributions.deleteConfirm', { id })}
        confirmLabel={t('common.delete')}
        loading={remove.isPending}
        error={remove.isError ? remove.error : undefined}
        onCancel={() => {
          setDeleting(false);
          remove.reset();
        }}
        onConfirm={() => {
          remove.mutate();
        }}
      />
    </Stack>
  );
}
