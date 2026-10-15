import {
  Alert,
  Anchor,
  Badge,
  Button,
  Code,
  Group,
  Paper,
  Skeleton,
  Stack,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { useDebouncedValue } from '@mantine/hooks';
import { notifications } from '@mantine/notifications';
import { IconAlertTriangle, IconDeviceFloppy } from '@tabler/icons-react';
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router';

import { CacheActions, usePermissions } from '@/auth/usePermissions';
import { issueMessage } from '@/components/cache/helpers';
import { PolicySpecFields } from '@/components/cache/PolicySpecFields';
import { ErrorAlert } from '@/components/iam/common';
import {
  builtinPolicies,
  cacheControlOf,
  cacheKeys,
  compactDuration,
  completeSpec,
  specOfPolicy,
  standardSpec,
  validatePolicy,
} from '@/lib/cache';
import { problemMessage } from '@/lib/problem';
import { isApiError, useServices, type CachePolicy, type CachePolicySpec } from '@/services';

import { CacheHeader } from './CacheHeader';
import { visibleCacheTabs } from './cacheTabs';

/** The policy as the editor shows it: complete, without the server fields. */
function editable(p: CachePolicy): CachePolicy {
  const { builtin: _b, etag: _e, updatedAt: _u, ...rest } = p;
  return { ...rest, ...completeSpec(specOfPolicy(p)) };
}

/**
 * The live preview of a policy (TD-C-02): the Cache-Control the clients
 * receive, computed as the user types, and the summary of the server
 * (POST /cache/preview) once the policy is valid.
 */
function PolicyPreview({ policy, valid }: { policy: CachePolicy; valid: boolean }) {
  const { t } = useTranslation();
  const { cache } = useServices();
  const header = cacheControlOf(policy);
  const [debounced] = useDebouncedValue(policy, 300);
  const preview = useQuery({
    queryKey: ['cache', 'preview', JSON.stringify(debounced)] as const,
    queryFn: () => cache.previewPolicy(debounced),
    enabled: valid,
    retry: false,
    staleTime: 60_000,
    placeholderData: keepPreviousData,
  });
  const ttl = policy.ttl ?? {};
  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('cache.policy.preview')}
      style={{ position: 'sticky', top: 72 }}
    >
      <Stack gap="xs">
        <Title order={4}>{t('cache.policy.preview')}</Title>
        <Text size="xs" c="dimmed">
          {t('cache.policy.previewHint')}
        </Text>
        <Text size="sm" fw={500} id="cache-control-preview-label">
          Cache-Control
        </Text>
        <div aria-live="polite" aria-labelledby="cache-control-preview-label">
          {header.inherited ? (
            <Text size="sm" c="dimmed" data-testid="cache-control-preview">
              {t('cache.policy.previewInherited')}
            </Text>
          ) : (
            <Code block data-testid="cache-control-preview">
              {header.value}
            </Code>
          )}
        </div>
        <Text size="sm">
          {policy.bypass?.always === true
            ? t('cache.policy.previewBypass')
            : t('cache.policy.previewTtl', {
                default: compactDuration(ttl.default),
                min: compactDuration(ttl.min),
                max: compactDuration(ttl.max),
              })}
        </Text>
        {!valid ? (
          <Text size="xs" c="red">
            {t('cache.policy.previewInvalid')}
          </Text>
        ) : preview.isError ? (
          <Text size="xs" c="red">
            {problemMessage(t, preview.error)}
          </Text>
        ) : preview.data !== undefined ? (
          <Text size="xs" c="dimmed" data-testid="policy-summary">
            {preview.data.summary}
          </Text>
        ) : null}
      </Stack>
    </Paper>
  );
}

/**
 * D-CCH-03: the editor of a cache policy (lifetimes, key, bypass, stale
 * windows, compression...), with the live preview of the Cache-Control
 * the clients receive and the validation as the user types (TTL min ≤
 * default ≤ max). A built-in policy is read-only: it can be duplicated.
 */
export function CachePolicyEditorPage() {
  const { t } = useTranslation();
  const { name: routeName } = useParams();
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { cache } = useServices();
  const perms = usePermissions();
  const canManage = perms.can(CacheActions.manage);
  const isNew = routeName === undefined;
  const from = params.get('from') ?? '';
  const sourceName = isNew ? from : routeName;
  const source = useQuery({
    queryKey: cacheKeys.policy(sourceName),
    queryFn: () => cache.getPolicy(sourceName),
    enabled: sourceName !== '' && canManage,
  });
  const [draft, setDraft] = useState<CachePolicy | null>(null);

  const initial: CachePolicy | undefined = isNew
    ? from === ''
      ? { name: '', ...standardSpec() }
      : source.data !== undefined
        ? {
            ...editable(source.data),
            name: `${source.data.name}-copy`,
            comment: source.data.comment,
          }
        : (() => {
            const b = builtinPolicies().find((p) => p.name === from);
            return b === undefined ? undefined : { ...editable(b), name: `${b.name}-copy` };
          })()
    : source.data !== undefined
      ? editable(source.data)
      : undefined;
  const current = draft ?? initial;
  const readOnly = !isNew && source.data?.builtin === true;
  const issues = current === undefined ? [] : validatePolicy(current);
  const nameIssue = issues.find((i) => i.field === 'name');

  const save = useMutation({
    mutationFn: (p: CachePolicy) =>
      isNew ? cache.createPolicy(p) : cache.updatePolicy(p, source.data?.etag),
    onSuccess: async (stored) => {
      queryClient.setQueryData(cacheKeys.policy(stored.name), stored);
      await queryClient.invalidateQueries({ queryKey: cacheKeys.policies });
      setDraft(null);
      notifications.show({
        color: 'green',
        message: isNew
          ? t('cache.policy.created', { name: stored.name })
          : t('cache.policy.saved', { name: stored.name }),
      });
      if (isNew)
        void navigate(`/cache/policies/${encodeURIComponent(stored.name)}`, { replace: true });
    },
  });
  const conflict = isApiError(save.error) && save.error.isPreconditionFailed;

  const tabs = visibleCacheTabs(perms);
  const header = <CacheHeader active="policies" tabs={tabs} />;

  if (perms.ready && !canManage) {
    return (
      <Stack gap="md">
        {header}
        <Alert color="gray" variant="light" role="alert">
          {t('permissions.missing', { action: CacheActions.manage })}
        </Alert>
      </Stack>
    );
  }
  if (source.isError) {
    return (
      <Stack gap="md">
        {header}
        <ErrorAlert
          error={source.error}
          title={
            isApiError(source.error) && source.error.isNotFound
              ? t('cache.policy.notFound', { name: sourceName })
              : t('cache.policies.loadError')
          }
        />
        <Anchor component={Link} to="/cache/policies">
          {t('cache.policy.back')}
        </Anchor>
      </Stack>
    );
  }
  if (current === undefined) {
    return (
      <Stack gap="md">
        {header}
        <Skeleton height={320} />
      </Stack>
    );
  }

  const change = (spec: CachePolicySpec) => {
    save.reset();
    setDraft({ ...current, ...spec, name: current.name, comment: current.comment });
  };
  const title = isNew
    ? t('cache.policy.newTitle')
    : t('cache.policy.editTitle', { name: current.name });

  return (
    <Stack gap="md">
      {header}
      <Group justify="space-between" align="flex-start" wrap="wrap">
        <Stack gap={2}>
          <Anchor component={Link} to="/cache/policies" size="sm">
            {t('cache.policy.back')}
          </Anchor>
          <Group gap="xs">
            <Title order={3}>{title}</Title>
            {readOnly ? (
              <Badge variant="light" color="gray">
                {t('cache.policies.builtin')}
              </Badge>
            ) : null}
          </Group>
        </Stack>
        {readOnly ? (
          <Button
            component={Link}
            to={`/cache/policies/-/new?from=${encodeURIComponent(current.name)}`}
            variant="light"
          >
            {t('cache.policies.duplicate')}
          </Button>
        ) : null}
      </Group>
      {readOnly ? (
        <Alert color="blue" variant="light">
          {t('cache.policy.builtinHint')}
        </Alert>
      ) : null}
      <Group align="flex-start" gap="md" wrap="wrap">
        <div style={{ flex: '2 1 560px', minWidth: 0 }}>
          <Stack
            gap="md"
            component="form"
            aria-label={title}
            onSubmit={(event) => {
              event.preventDefault();
              if (issues.length === 0 && !readOnly) save.mutate(current);
            }}
          >
            <Paper withBorder radius="md" p="md">
              <Group grow align="flex-start" wrap="wrap">
                <TextInput
                  label={t('cache.policy.name')}
                  description={t('cache.policy.nameHint')}
                  value={current.name}
                  required
                  disabled={!isNew}
                  error={
                    current.name !== '' && nameIssue !== undefined
                      ? issueMessage(t, nameIssue)
                      : undefined
                  }
                  onChange={(event) => {
                    save.reset();
                    setDraft({ ...current, name: event.currentTarget.value.trim() });
                  }}
                />
                <TextInput
                  label={t('cache.policy.comment')}
                  value={current.comment ?? ''}
                  disabled={readOnly}
                  onChange={(event) => {
                    save.reset();
                    const comment = event.currentTarget.value;
                    setDraft({ ...current, comment: comment === '' ? undefined : comment });
                  }}
                />
              </Group>
            </Paper>
            <PolicySpecFields
              spec={specOfPolicy(current)}
              issues={issues}
              disabled={readOnly}
              onChange={change}
            />
            {conflict ? (
              <Alert
                color="yellow"
                variant="light"
                icon={<IconAlertTriangle size={18} />}
                title={t('cache.tab.conflictTitle')}
              >
                {t('cache.policy.conflict')}
              </Alert>
            ) : save.isError ? (
              <Alert color="red" variant="light" role="alert">
                {problemMessage(t, save.error)}
              </Alert>
            ) : null}
            {!readOnly ? (
              <Group justify="flex-end" gap="xs">
                <Button
                  variant="default"
                  onClick={() => {
                    setDraft(null);
                    save.reset();
                    if (conflict) void source.refetch();
                  }}
                >
                  {conflict ? t('cache.tab.reload') : t('cache.tab.discard')}
                </Button>
                <Button
                  type="submit"
                  leftSection={<IconDeviceFloppy size={16} />}
                  loading={save.isPending}
                  disabled={issues.length > 0 || conflict || (!isNew && draft === null)}
                >
                  {isNew ? t('cache.policy.create') : t('cache.tab.save')}
                </Button>
              </Group>
            ) : null}
            {issues.length > 0 && current.name !== '' ? (
              <Text size="sm" c="red" role="status">
                {t('cache.policy.issues', { n: issues.length })}
              </Text>
            ) : null}
          </Stack>
        </div>
        <div style={{ flex: '1 1 280px', minWidth: 0, alignSelf: 'stretch' }}>
          <PolicyPreview
            policy={current}
            valid={issues.length === 0 || (issues.length === 1 && nameIssue !== undefined)}
          />
        </div>
      </Group>
    </Stack>
  );
}
