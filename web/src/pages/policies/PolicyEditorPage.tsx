import {
  Alert,
  Anchor,
  Badge,
  Breadcrumbs,
  Button,
  Group,
  Loader,
  Paper,
  SegmentedControl,
  Select,
  Stack,
  Tabs,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconCircleCheck, IconLock, IconUnlink } from '@tabler/icons-react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router';

import { usePermissions } from '@/auth/usePermissions';
import { ErrorAlert } from '@/components/iam/common';
import { PolicyBuilder } from '@/components/policy/PolicyBuilder';
import { PolicyJsonEditor } from '@/components/policy/PolicyJsonEditor';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { formatJson, parseJson } from '@/lib/json';
import { emptyPolicyDocument } from '@/lib/policyBuilder';
import { isApiError, useServices, type LocatedError, type Policy } from '@/services';

const policyNamePattern = /^[A-Za-z0-9+=,.@_-]{1,128}$/;

/** Route /policies/new and /policies/:name. */
export function PolicyEditorPage() {
  const { t } = useTranslation();
  const { name } = useParams();
  const { policies } = useServices();
  const query = useQuery({
    queryKey: iamKeys.policy(name ?? ''),
    queryFn: () => policies.get(name ?? ''),
    enabled: name !== undefined,
  });

  if (name === undefined) {
    return <PolicyEditor policy={undefined} />;
  }
  if (query.isError) {
    return (
      <Stack gap="lg">
        <EditorBreadcrumbs current={name} />
        <ErrorAlert
          error={query.error}
          title={t('policyEditor.loadError', { policy: name })}
          onRetry={() => {
            void query.refetch();
          }}
        />
      </Stack>
    );
  }
  if (query.isPending) {
    return <Loader aria-label={t('common.loading')} />;
  }
  return <PolicyEditor key={`${query.data.name}:${query.data.updatedAt}`} policy={query.data} />;
}

function EditorBreadcrumbs({ current }: { current: string }) {
  const { t } = useTranslation();
  return (
    <Breadcrumbs>
      <Anchor component={Link} to="/policies">
        {t('nav.policies')}
      </Anchor>
      <Text>{current}</Text>
    </Breadcrumbs>
  );
}

/** Editor of a new (policy undefined) or existing policy. */
export function PolicyEditor({ policy }: { policy: Policy | undefined }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { policies } = useServices();
  const { can, ready } = usePermissions();
  const isNew = policy === undefined;
  // Unknown permissions do not lock the editor: the server decides on save.
  const readOnly = policy?.builtIn === true || (ready && !can('admin:CreatePolicy'));

  const [name, setName] = useState('');
  const [nameTouched, setNameTouched] = useState(false);
  const [text, setText] = useState(() => formatJson(policy?.document ?? emptyPolicyDocument()));
  const [serverErrors, setServerErrors] = useState<LocatedError[]>([]);
  const [validated, setValidated] = useState(false);
  const [tab, setTab] = useState<string | null>('json');

  const parsed = useMemo(() => parseJson(text), [text]);
  const nameInvalid = isNew && !policyNamePattern.test(name);
  const policyName = policy?.name ?? name;

  const changeText = (next: string) => {
    setText(next);
    setServerErrors([]);
    setValidated(false);
  };

  const validate = useMutation({
    mutationFn: () => policies.validate({ document: text }),
    onSuccess: (report) => {
      setServerErrors(report.errors);
      setValidated(report.valid);
    },
  });

  const save = useInvalidatingMutation({
    mutationFn: () =>
      isNew
        ? policies.create({ name, document: text })
        : policies.save({ name: policyName, document: text }),
    invalidate: [iamKeys.policies, iamKeys.permissions],
    onSuccess: async (saved) => {
      notifications.show({
        color: 'teal',
        message: t('policyEditor.saved', { policy: saved.name }),
      });
      if (isNew) {
        await navigate(`/policies/${encodeURIComponent(saved.name)}`, { replace: true });
      }
    },
    onError: (error) => {
      if (isApiError(error) && error.errors.length > 0) {
        setServerErrors([...error.errors]);
      }
    },
  });

  const syntaxValid = parsed.ok;
  const canSubmit = syntaxValid && !readOnly && !(isNew && nameInvalid);

  return (
    <Stack gap="lg">
      <EditorBreadcrumbs current={isNew ? t('policyEditor.newTitle') : policyName} />
      <Group justify="space-between" align="flex-start" wrap="wrap">
        <Group gap="xs">
          <Title order={2}>{isNew ? t('policyEditor.newTitle') : policyName}</Title>
          {policy?.builtIn ? (
            <Badge variant="light" color="gray" leftSection={<IconLock size={12} />}>
              {t('policies.builtIn')}
            </Badge>
          ) : null}
        </Group>
        {!readOnly ? (
          <Group gap="xs">
            <Button
              variant="default"
              onClick={() => {
                validate.mutate();
              }}
              loading={validate.isPending}
              disabled={!syntaxValid}
            >
              {t('policyEditor.validate')}
            </Button>
            <Button
              onClick={() => {
                setNameTouched(true);
                if (canSubmit) save.mutate();
              }}
              loading={save.isPending}
              disabled={!syntaxValid}
            >
              {t('common.save')}
            </Button>
          </Group>
        ) : null}
      </Group>

      {policy?.builtIn ? (
        <Alert variant="light" color="gray">
          {t('policyEditor.builtInNotice')}
        </Alert>
      ) : null}
      {save.isError && !(isApiError(save.error) && save.error.errors.length > 0) ? (
        <ErrorAlert error={save.error} title={t('policyEditor.saveError')} />
      ) : null}
      {save.isError && isApiError(save.error) && save.error.errors.length > 0 ? (
        <Alert color="red" variant="light" role="alert">
          {t('policyEditor.invalid', { count: save.error.errors.length })}
        </Alert>
      ) : null}
      {validate.isError ? <ErrorAlert error={validate.error} /> : null}
      {validated ? (
        <Alert color="teal" variant="light" icon={<IconCircleCheck size={18} />}>
          {t('policyEditor.valid')}
        </Alert>
      ) : null}
      {!syntaxValid ? (
        <Alert color="red" variant="light" role="alert">
          {t('policyEditor.syntaxError', {
            line: parsed.error.line,
            column: parsed.error.column,
            message: parsed.error.message,
          })}
        </Alert>
      ) : null}

      {isNew ? (
        <TextInput
          label={t('common.name')}
          description={t('policyEditor.nameHint')}
          required
          value={name}
          onChange={(event) => {
            setName(event.currentTarget.value);
          }}
          onBlur={() => {
            setNameTouched(true);
          }}
          error={nameTouched && nameInvalid ? t('policyEditor.nameInvalid') : undefined}
          maw={420}
        />
      ) : null}

      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List>
          <Tabs.Tab value="json">{t('policyEditor.tabs.json')}</Tabs.Tab>
          <Tabs.Tab value="builder">{t('policyEditor.tabs.builder')}</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="json" pt="md">
          <PolicyJsonEditor
            label={t('policyEditor.document')}
            description={t('policyEditor.documentHint')}
            value={text}
            onChange={changeText}
            serverErrors={serverErrors}
            readOnly={readOnly}
            height={440}
          />
        </Tabs.Panel>
        <Tabs.Panel value="builder" pt="md">
          <PolicyBuilder
            document={parsed.ok ? parsed.value : undefined}
            onChange={(doc) => {
              changeText(formatJson(doc));
            }}
            readOnly={readOnly}
          />
        </Tabs.Panel>
      </Tabs>

      {!isNew && policy ? <PolicyEntitiesPanel policy={policy.name} /> : null}
    </Stack>
  );
}

/** Users and groups a policy is attached to, with attach and detach. */
function PolicyEntitiesPanel({ policy }: { policy: string }) {
  const { t } = useTranslation();
  const services = useServices();
  const { can } = usePermissions();
  const [kind, setKind] = useState<'user' | 'group'>('user');
  const [target, setTarget] = useState<string | null>(null);
  const canAttach = can('admin:AttachUserOrGroupPolicy');

  const entities = useQuery({
    queryKey: iamKeys.policyEntities(policy),
    queryFn: () => services.policies.entities(policy),
  });
  const users = useQuery({
    queryKey: iamKeys.users,
    queryFn: () => services.users.list(),
    enabled: canAttach && can('admin:ListUsers'),
  });
  const groups = useQuery({
    queryKey: iamKeys.groups,
    queryFn: () => services.groups.list(),
    enabled: canAttach && can('admin:ListGroups'),
  });
  const invalidate = [iamKeys.policies, iamKeys.users, iamKeys.groups, iamKeys.permissions];
  const attach = useInvalidatingMutation({
    mutationFn: () =>
      services.policies.attach({
        policyNames: [policy],
        ...(kind === 'user' ? { user: target ?? '' } : { group: target ?? '' }),
      }),
    invalidate,
    onSuccess: () => {
      setTarget(null);
    },
  });
  const detach = useInvalidatingMutation({
    mutationFn: (entity: { user?: string; group?: string }) =>
      services.policies.detach({ policyNames: [policy], ...entity }),
    invalidate,
  });

  const attached = kind === 'user' ? (entities.data?.users ?? []) : (entities.data?.groups ?? []);
  const options = (
    kind === 'user'
      ? (users.data ?? []).map((u) => u.accessKey)
      : (groups.data ?? []).map((g) => g.name)
  ).filter((n) => !attached.includes(n));

  return (
    <Paper
      withBorder
      p="md"
      radius="md"
      component="section"
      aria-label={t('policyEditor.entities.title')}
    >
      <Stack gap="sm">
        <Title order={3}>{t('policyEditor.entities.title')}</Title>
        {entities.isError ? <ErrorAlert error={entities.error} /> : null}
        {attach.isError ? <ErrorAlert error={attach.error} /> : null}
        {detach.isError ? <ErrorAlert error={detach.error} /> : null}
        {entities.data ? (
          <Stack gap="xs">
            <EntityRow
              label={t('policyEditor.entities.users')}
              names={entities.data.users}
              canDetach={canAttach}
              onDetach={(user) => {
                detach.mutate({ user });
              }}
            />
            <EntityRow
              label={t('policyEditor.entities.groups')}
              names={entities.data.groups}
              canDetach={canAttach}
              onDetach={(group) => {
                detach.mutate({ group });
              }}
            />
          </Stack>
        ) : null}
        {canAttach ? (
          <Group align="flex-end" gap="xs" wrap="wrap">
            <SegmentedControl
              aria-label={t('policyEditor.entities.kind')}
              value={kind}
              onChange={(value) => {
                setKind(value === 'group' ? 'group' : 'user');
                setTarget(null);
              }}
              data={[
                { value: 'user', label: t('policyEditor.entities.user') },
                { value: 'group', label: t('policyEditor.entities.group') },
              ]}
            />
            <Select
              label={t('policyEditor.entities.attachTo')}
              data={options}
              value={target}
              onChange={setTarget}
              searchable
              w={240}
            />
            <Button
              variant="light"
              disabled={target === null}
              loading={attach.isPending}
              onClick={() => {
                attach.mutate();
              }}
            >
              {t('policyEditor.entities.attach')}
            </Button>
          </Group>
        ) : null}
      </Stack>
    </Paper>
  );
}

function EntityRow({
  label,
  names,
  canDetach,
  onDetach,
}: {
  label: string;
  names: string[];
  canDetach: boolean;
  onDetach: (name: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <Group gap="xs">
      <Text size="sm" fw={500}>
        {label}
      </Text>
      {names.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t('permissions.none')}
        </Text>
      ) : (
        names.map((name) => (
          <Badge
            key={name}
            variant="light"
            rightSection={
              canDetach ? (
                <Anchor
                  component="button"
                  type="button"
                  aria-label={t('policyEditor.entities.detach', { name })}
                  onClick={() => {
                    onDetach(name);
                  }}
                  style={{ display: 'flex' }}
                >
                  <IconUnlink size={12} />
                </Anchor>
              ) : undefined
            }
          >
            {name}
          </Badge>
        ))
      )}
    </Group>
  );
}
