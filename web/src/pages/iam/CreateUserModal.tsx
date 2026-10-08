import {
  ActionIcon,
  Button,
  CopyButton,
  Group,
  Modal,
  MultiSelect,
  PasswordInput,
  Stack,
  TextInput,
  Tooltip,
} from '@mantine/core';
import { IconCheck, IconCopy, IconRefresh } from '@tabler/icons-react';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { ErrorAlert } from '@/components/iam/common';
import {
  accessKeyMaxLength,
  accessKeyMinLength,
  checkAccessKey,
  checkSecretKey,
  generateAccessKey,
  generateSecretKey,
  secretKeyMaxLength,
  secretKeyMinLength,
} from '@/lib/credentials';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { useServices, type User } from '@/services';

export interface CreateUserModalProps {
  opened: boolean;
  onClose: () => void;
  policyNames: string[];
  groupNames: string[];
  onCreated?: (user: User) => void;
}

/** Form creating an IAM user with generated or chosen credentials. */
export function CreateUserModal({
  opened,
  onClose,
  policyNames,
  groupNames,
  onCreated,
}: CreateUserModalProps) {
  const { t } = useTranslation();
  const { users } = useServices();
  const [accessKey, setAccessKey] = useState('');
  const [secretKey, setSecretKey] = useState('');
  const [policies, setPolicies] = useState<string[]>([]);
  const [groups, setGroups] = useState<string[]>([]);
  const [touched, setTouched] = useState(false);

  const mutation = useInvalidatingMutation({
    mutationFn: () =>
      users.create({ accessKey: accessKey.trim(), secretKey, policyNames: policies, groups }),
    invalidate: [iamKeys.users, iamKeys.groups, iamKeys.permissions],
    onSuccess: (user) => {
      onCreated?.(user);
      close();
    },
  });

  const accessProblem = checkAccessKey(accessKey.trim());
  const secretProblem = checkSecretKey(secretKey);

  function close() {
    setAccessKey('');
    setSecretKey('');
    setPolicies([]);
    setGroups([]);
    setTouched(false);
    mutation.reset();
    onClose();
  }

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setTouched(true);
    if (accessProblem === undefined && secretProblem === undefined) {
      mutation.mutate();
    }
  };

  const limits = {
    min: accessKeyMinLength,
    max: accessKeyMaxLength,
    secretMin: secretKeyMinLength,
    secretMax: secretKeyMaxLength,
  };

  return (
    <Modal opened={opened} onClose={close} title={t('users.create.title')} size="lg">
      <form onSubmit={submit} noValidate aria-label={t('users.create.title')}>
        <Stack gap="md">
          {mutation.isError ? <ErrorAlert error={mutation.error} /> : null}
          <TextInput
            label={t('users.fields.accessKey')}
            description={t('users.create.accessKeyHint', limits)}
            required
            value={accessKey}
            onChange={(event) => {
              setAccessKey(event.currentTarget.value);
            }}
            error={
              touched && accessProblem !== undefined
                ? t(`users.validation.accessKey.${accessProblem}`, limits)
                : undefined
            }
            data-autofocus
            rightSection={
              <Tooltip label={t('users.create.generate')}>
                <ActionIcon
                  variant="subtle"
                  aria-label={t('users.create.generateAccessKey')}
                  onClick={() => {
                    setAccessKey(generateAccessKey());
                  }}
                >
                  <IconRefresh size={16} />
                </ActionIcon>
              </Tooltip>
            }
          />
          <Group align="flex-start" gap="xs" wrap="nowrap">
            <PasswordInput
              style={{ flex: 1 }}
              label={t('users.fields.secretKey')}
              description={t('users.create.secretKeyHint', limits)}
              required
              autoComplete="new-password"
              value={secretKey}
              onChange={(event) => {
                setSecretKey(event.currentTarget.value);
              }}
              error={
                touched && secretProblem !== undefined
                  ? t(`users.validation.secretKey.${secretProblem}`, limits)
                  : undefined
              }
            />
            <Stack gap={4} pt={46}>
              <Tooltip label={t('users.create.generate')}>
                <ActionIcon
                  variant="default"
                  size="lg"
                  aria-label={t('users.create.generateSecretKey')}
                  onClick={() => {
                    setSecretKey(generateSecretKey());
                  }}
                >
                  <IconRefresh size={16} />
                </ActionIcon>
              </Tooltip>
            </Stack>
            <Stack gap={4} pt={46}>
              <CopyButton value={secretKey}>
                {({ copied, copy }) => (
                  <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
                    <ActionIcon
                      variant="default"
                      size="lg"
                      aria-label={t('users.create.copySecretKey')}
                      onClick={copy}
                      disabled={secretKey === ''}
                    >
                      {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                    </ActionIcon>
                  </Tooltip>
                )}
              </CopyButton>
            </Stack>
          </Group>
          <MultiSelect
            label={t('users.fields.policies')}
            data={policyNames}
            value={policies}
            onChange={setPolicies}
            searchable
            clearable
          />
          <MultiSelect
            label={t('users.fields.groups')}
            data={groupNames}
            value={groups}
            onChange={setGroups}
            searchable
            clearable
          />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={close} disabled={mutation.isPending}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={mutation.isPending}>
              {t('common.create')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
