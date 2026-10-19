import {
  Alert,
  Button,
  Group,
  Modal,
  NumberInput,
  Select,
  Stack,
  Switch,
  TextInput,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconAlertTriangle, IconMovie } from '@tabler/icons-react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router';

import { bucketNameIssue } from '@/lib/bucketName';
import { browsePath } from '@/lib/objectPath';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { sizeUnits, toBytes, type SizeUnit } from '@/lib/units';
import { isApiError, useServices, type CreateBucketInput } from '@/services';

export interface CreateBucketDialogProps {
  opened: boolean;
  onClose: () => void;
}

/**
 * D-BKT-01 / TD-U-04: creation with live DNS validation, versioning, lock,
 * quota and the HLS video option (the media rule "hls").
 */
export function CreateBucketDialog({ opened, onClose }: CreateBucketDialogProps) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [touched, setTouched] = useState(false);
  const [versioning, setVersioning] = useState(false);
  const [objectLock, setObjectLock] = useState(false);
  const [hls, setHls] = useState(false);
  const [quota, setQuota] = useState<number | string>('');
  const [unit, setUnit] = useState<SizeUnit>('GiB');

  const issue = bucketNameIssue(name);
  const mutation = useMutation({
    mutationFn: (input: CreateBucketInput) => buckets.create(input),
    onSuccess: async (bucket) => {
      await queryClient.invalidateQueries({ queryKey: storageKeys.buckets });
      await queryClient.invalidateQueries({ queryKey: storageKeys.usage });
      notifications.show({
        color: 'green',
        message: t('buckets.created', { name: bucket.name }),
      });
      close();
      await navigate(browsePath(bucket.name));
    },
  });

  function close() {
    setName('');
    setTouched(false);
    setVersioning(false);
    setObjectLock(false);
    setHls(false);
    setQuota('');
    setUnit('GiB');
    mutation.reset();
    onClose();
  }

  const nameTaken = isApiError(mutation.error) && mutation.error.code === 'bucket_already_exists';
  const quotaValue = typeof quota === 'number' ? quota : Number.NaN;
  const quotaInvalid = quota !== '' && (!Number.isFinite(quotaValue) || quotaValue < 0);

  const nameError = nameTaken
    ? t('storageErrors.bucket_already_exists')
    : touched && issue !== null
      ? t(`buckets.nameIssues.${issue}`)
      : undefined;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setTouched(true);
    if (issue !== null || quotaInvalid) {
      return;
    }
    const input: CreateBucketInput = { name };
    if (versioning || objectLock) {
      input.versioning = true;
    }
    if (objectLock) {
      input.objectLock = true;
    }
    if (Number.isFinite(quotaValue) && quotaValue > 0) {
      input.quotaBytes = toBytes(quotaValue, unit);
    }
    if (hls) {
      input.hls = true;
    }
    mutation.mutate(input);
  };

  return (
    <Modal opened={opened} onClose={close} title={t('buckets.createTitle')} centered>
      <form onSubmit={submit} noValidate>
        <Stack gap="md">
          <TextInput
            label={t('buckets.name')}
            description={t('buckets.nameHint')}
            placeholder="my-bucket"
            required
            data-autofocus
            value={name}
            onChange={(event) => {
              setName(event.currentTarget.value);
              setTouched(true);
              if (mutation.isError) {
                mutation.reset();
              }
            }}
            error={nameError}
            autoComplete="off"
            spellCheck={false}
          />
          <Switch
            label={t('buckets.versioning')}
            description={t('buckets.versioningHint')}
            checked={versioning || objectLock}
            disabled={objectLock}
            onChange={(event) => {
              setVersioning(event.currentTarget.checked);
            }}
          />
          <Switch
            label={t('buckets.objectLock')}
            description={t('buckets.objectLockHint')}
            checked={objectLock}
            onChange={(event) => {
              setObjectLock(event.currentTarget.checked);
            }}
          />
          {objectLock ? (
            <Alert color="orange" variant="light" icon={<IconAlertTriangle size={18} />}>
              {t('buckets.objectLockWarning')}
            </Alert>
          ) : null}
          <Switch
            label={
              <Group gap={6} wrap="nowrap">
                <IconMovie size={16} aria-hidden />
                {t('buckets.hls')}
              </Group>
            }
            description={t('buckets.hlsHint')}
            checked={hls}
            onChange={(event) => {
              setHls(event.currentTarget.checked);
            }}
          />
          <Group align="flex-end" gap="xs" wrap="nowrap">
            <NumberInput
              label={t('buckets.quota')}
              description={t('buckets.quotaHint')}
              min={0}
              decimalScale={2}
              value={quota}
              onChange={setQuota}
              error={quotaInvalid ? t('buckets.quotaInvalid') : undefined}
              style={{ flex: 1 }}
            />
            <Select
              aria-label={t('buckets.quotaUnit')}
              data={[...sizeUnits]}
              value={unit}
              allowDeselect={false}
              onChange={(value) => {
                if (value !== null) {
                  setUnit(value as SizeUnit);
                }
              }}
              w={100}
            />
          </Group>
          {mutation.isError && !nameTaken ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, mutation.error)}
            </Alert>
          ) : null}
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>
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
