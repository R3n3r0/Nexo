import {
  Alert,
  Button,
  Checkbox,
  Group,
  Paper,
  Select,
  SimpleGrid,
  Stack,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconFirstAidKit } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { healKeys } from '@/lib/cluster';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type HealJob, type HealRequest } from '@/services';

export interface StartHealFormProps {
  /** Called with the job the server started (or the identical one running). */
  onStarted: (job: HealJob) => void;
  disabled?: boolean;
}

/**
 * Starts a healing job: the whole deployment (format and every bucket) or
 * one bucket, optionally a prefix and its objects (recursive), as a dry
 * run, with the deep bitrot verification, removing the dangling objects.
 */
export function StartHealForm({ onStarted, disabled = false }: StartHealFormProps) {
  const { t } = useTranslation();
  const { heal, buckets } = useServices();
  const queryClient = useQueryClient();
  const [bucket, setBucket] = useState<string | null>(null);
  const [prefix, setPrefix] = useState('');
  const [recursive, setRecursive] = useState(true);
  const [dryRun, setDryRun] = useState(false);
  const [deep, setDeep] = useState(false);
  const [remove, setRemove] = useState(false);

  const bucketList = useQuery({
    queryKey: storageKeys.buckets,
    queryFn: () => buckets.list(),
    enabled: !disabled,
  });
  const start = useMutation({
    mutationFn: (request: HealRequest) => heal.start(request),
    onSuccess: async (job) => {
      onStarted(job);
      await queryClient.invalidateQueries({ queryKey: healKeys.jobs });
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const request: HealRequest = { recursive, dryRun, deep, remove };
    if (bucket !== null && bucket !== '') {
      request.bucket = bucket;
      if (prefix !== '') {
        request.prefix = prefix;
      }
    }
    start.mutate(request);
  };

  const title = t('healing.start.title');
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <form onSubmit={submit}>
        <Stack gap="md">
          <Stack gap={2}>
            <Title order={4}>{title}</Title>
            <Text size="sm" c="dimmed">
              {t('healing.start.hint')}
            </Text>
          </Stack>
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md">
            <Select
              label={t('healing.start.bucket')}
              placeholder={t('healing.start.allBuckets')}
              data={(bucketList.data ?? []).map((b) => b.name)}
              value={bucket}
              onChange={(value) => {
                setBucket(value);
                if (value === null) setPrefix('');
              }}
              searchable
              clearable
              disabled={disabled}
              nothingFoundMessage={t('healing.start.noBuckets')}
            />
            <TextInput
              label={t('healing.start.prefix')}
              placeholder={t('healing.start.prefixPlaceholder')}
              value={prefix}
              onChange={(e) => {
                setPrefix(e.currentTarget.value);
              }}
              disabled={disabled || bucket === null}
            />
          </SimpleGrid>
          <Group gap="lg" wrap="wrap">
            <Checkbox
              label={t('healing.options.recursive')}
              checked={recursive}
              onChange={(e) => {
                setRecursive(e.currentTarget.checked);
              }}
              disabled={disabled}
            />
            <Checkbox
              label={t('healing.options.dryRun')}
              checked={dryRun}
              onChange={(e) => {
                setDryRun(e.currentTarget.checked);
              }}
              disabled={disabled}
            />
            <Checkbox
              label={t('healing.options.deep')}
              checked={deep}
              onChange={(e) => {
                setDeep(e.currentTarget.checked);
              }}
              disabled={disabled}
            />
            <Checkbox
              label={t('healing.options.remove')}
              description={t('healing.options.removeHint')}
              checked={remove}
              onChange={(e) => {
                setRemove(e.currentTarget.checked);
              }}
              disabled={disabled}
            />
          </Group>
          {start.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, start.error)}
            </Alert>
          ) : null}
          <Group>
            <Button
              type="submit"
              leftSection={<IconFirstAidKit size={16} />}
              loading={start.isPending}
              disabled={disabled}
            >
              {t('healing.start.submit')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Paper>
  );
}
