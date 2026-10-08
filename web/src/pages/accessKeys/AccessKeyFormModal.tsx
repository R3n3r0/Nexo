import '@mantine/dates/styles.css';

import { Button, Group, Modal, Select, Stack, Switch, Textarea, TextInput } from '@mantine/core';
import { DateTimePicker } from '@mantine/dates';
import { useMemo, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { ErrorAlert } from '@/components/iam/common';
import { PolicyJsonEditor } from '@/components/policy/PolicyJsonEditor';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { formatJson } from '@/lib/json';
import { emptyPolicyDocument } from '@/lib/policyBuilder';
import { lintPolicy } from '@/lib/policyLint';
import {
  isApiError,
  useServices,
  type AccessKey,
  type CreatedAccessKey,
  type LocatedError,
  type PolicyDocument,
} from '@/services';

export interface AccessKeyFormModalProps {
  opened: boolean;
  onClose: () => void;
  /** The key to edit; undefined creates a new one. */
  accessKey?: AccessKey;
  /** Users the caller may create keys for (admins); empty for self-service. */
  userOptions?: string[];
  /** Default owner of a new key. */
  defaultUser?: string;
  onCreated?: (created: CreatedAccessKey) => void;
}

/** Create or edit an access key: name, description, expiration, status, inline policy. */
export function AccessKeyFormModal({
  opened,
  onClose,
  accessKey,
  userOptions = [],
  defaultUser,
  onCreated,
}: AccessKeyFormModalProps) {
  const { t } = useTranslation();
  const editing = accessKey !== undefined;
  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={
        editing
          ? t('accessKeys.form.editTitle', { key: accessKey.accessKey })
          : t('accessKeys.form.createTitle')
      }
      size="xl"
    >
      {opened ? (
        <AccessKeyForm
          {...(accessKey !== undefined ? { accessKey } : {})}
          userOptions={userOptions}
          {...(defaultUser !== undefined ? { defaultUser } : {})}
          onDone={onClose}
          {...(onCreated !== undefined ? { onCreated } : {})}
        />
      ) : null}
    </Modal>
  );
}

function AccessKeyForm({
  accessKey,
  userOptions,
  defaultUser,
  onDone,
  onCreated,
}: {
  accessKey?: AccessKey;
  userOptions: string[];
  defaultUser?: string;
  onDone: () => void;
  onCreated?: (created: CreatedAccessKey) => void;
}) {
  const { t } = useTranslation();
  const { accessKeys } = useServices();
  const editing = accessKey !== undefined;
  const [owner, setOwner] = useState<string | null>(defaultUser ?? null);
  const [name, setName] = useState(accessKey?.name ?? '');
  const [description, setDescription] = useState(accessKey?.description ?? '');
  const [enabled, setEnabled] = useState(accessKey?.status !== 'disabled');
  const [expires, setExpires] = useState(accessKey?.expiration !== undefined);
  const [expiration, setExpiration] = useState<Date | null>(
    accessKey?.expiration !== undefined ? new Date(accessKey.expiration) : null,
  );
  const [withPolicy, setWithPolicy] = useState(accessKey?.policy !== undefined);
  const [policyText, setPolicyText] = useState(() =>
    formatJson(accessKey?.policy ?? emptyPolicyDocument()),
  );
  const [serverErrors, setServerErrors] = useState<LocatedError[]>([]);
  const [touched, setTouched] = useState(false);

  const policyErrors = useMemo(
    () => (withPolicy ? lintPolicy(policyText) : []),
    [withPolicy, policyText],
  );
  const now = useMemo(() => new Date(), []);
  const expirationProblem = expires
    ? expiration === null
      ? 'required'
      : expiration.getTime() <= Date.now()
        ? 'past'
        : undefined
    : undefined;

  const mutation = useInvalidatingMutation<AccessKey | CreatedAccessKey>({
    mutationFn: () => {
      const policy = withPolicy ? (JSON.parse(policyText) as PolicyDocument) : undefined;
      if (accessKey !== undefined) {
        return accessKeys.update(accessKey.accessKey, {
          name,
          description,
          status: enabled ? 'enabled' : 'disabled',
          ...(expires && expiration !== null
            ? { expiration: expiration.toISOString() }
            : accessKey.expiration !== undefined
              ? { clearExpiration: true }
              : {}),
          ...(policy !== undefined
            ? { policy }
            : accessKey.policy !== undefined
              ? { clearPolicy: true }
              : {}),
        });
      }
      return accessKeys.create({
        ...(owner !== null && owner !== '' ? { parentUser: owner } : {}),
        name,
        description,
        ...(expires && expiration !== null ? { expiration: expiration.toISOString() } : {}),
        ...(policy !== undefined ? { policy } : {}),
      });
    },
    invalidate: [iamKeys.accessKeys],
    onSuccess: (result) => {
      if (!editing && 'secretKey' in result) {
        onCreated?.(result);
      }
      onDone();
    },
    onError: (error) => {
      if (isApiError(error) && error.errors.length > 0) {
        setServerErrors([...error.errors]);
      }
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setTouched(true);
    if (expirationProblem !== undefined || policyErrors.length > 0) return;
    mutation.mutate();
  };

  return (
    <form
      onSubmit={submit}
      noValidate
      aria-label={
        accessKey !== undefined
          ? t('accessKeys.form.editTitle', { key: accessKey.accessKey })
          : t('accessKeys.form.createTitle')
      }
    >
      <Stack gap="md">
        {mutation.isError ? <ErrorAlert error={mutation.error} /> : null}
        {!editing && userOptions.length > 0 ? (
          <Select
            label={t('accessKeys.fields.owner')}
            description={t('accessKeys.form.ownerHint')}
            data={userOptions}
            value={owner}
            onChange={setOwner}
            searchable
            clearable
          />
        ) : null}
        <TextInput
          label={t('accessKeys.fields.name')}
          value={name}
          maxLength={64}
          onChange={(event) => {
            setName(event.currentTarget.value);
          }}
          data-autofocus
        />
        <Textarea
          label={t('accessKeys.fields.description')}
          value={description}
          maxLength={256}
          autosize
          minRows={2}
          onChange={(event) => {
            setDescription(event.currentTarget.value);
          }}
        />
        {editing ? (
          <Switch
            label={t('accessKeys.fields.enabled')}
            checked={enabled}
            onChange={(event) => {
              setEnabled(event.currentTarget.checked);
            }}
          />
        ) : null}
        <Switch
          label={t('accessKeys.form.expires')}
          checked={expires}
          onChange={(event) => {
            setExpires(event.currentTarget.checked);
          }}
        />
        {expires ? (
          <DateTimePicker
            label={t('accessKeys.fields.expiration')}
            value={expiration}
            onChange={setExpiration}
            minDate={now}
            required
            error={
              touched && expirationProblem !== undefined
                ? t(`accessKeys.form.expiration.${expirationProblem}`)
                : undefined
            }
          />
        ) : null}
        <Switch
          label={t('accessKeys.form.inlinePolicy')}
          description={t('accessKeys.form.inlinePolicyHint')}
          checked={withPolicy}
          onChange={(event) => {
            setWithPolicy(event.currentTarget.checked);
          }}
        />
        {withPolicy ? (
          <PolicyJsonEditor
            label={t('accessKeys.fields.policy')}
            value={policyText}
            onChange={(text) => {
              setPolicyText(text);
              setServerErrors([]);
            }}
            serverErrors={serverErrors}
            height={260}
          />
        ) : null}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onDone} disabled={mutation.isPending}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" loading={mutation.isPending}>
            {editing ? t('common.save') : t('common.create')}
          </Button>
        </Group>
      </Stack>
    </form>
  );
}
