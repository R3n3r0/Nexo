import {
  Alert,
  Badge,
  Group,
  Paper,
  SegmentedControl,
  Skeleton,
  Stack,
  Switch,
  Text,
  Title,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconShieldLock } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { StorageActions } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import { accessKeys } from '@/lib/accessQueries';
import { problemMessage } from '@/lib/problem';
import { useServices, type BlockPublic, type BlockPublicInput } from '@/services';

export interface BlockPublicControlProps {
  /** The bucket; absent for the cluster level (with its mode). */
  bucket?: string;
  canEdit: boolean;
}

/**
 * D-ACC-04: the block public access of a bucket or of the whole cluster.
 * While it is on, the allow rules for everyone are ignored (and, in the
 * reject mode of the cluster, refused when saved); deny rules still apply.
 */
export function BlockPublicControl({ bucket, canEdit }: BlockPublicControlProps) {
  const { t } = useTranslation();
  const { accessRules } = useServices();
  const queryClient = useQueryClient();
  const key = accessKeys.blockPublic(bucket);
  const query = useQuery({ queryKey: key, queryFn: () => accessRules.getBlockPublic(bucket) });
  const mutation = useMutation({
    mutationFn: (input: BlockPublicInput) => accessRules.setBlockPublic(input, bucket),
    onSuccess: async (stored: BlockPublic) => {
      queryClient.setQueryData(key, stored);
      // Every bucket may change: its rules, its public state, the badges of the list.
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: accessKeys.all }),
        queryClient.invalidateQueries({ queryKey: ['buckets'] }),
      ]);
      notifications.show({
        color: 'green',
        message: stored.enabled ? t('blockPublic.turnedOn') : t('blockPublic.turnedOff'),
      });
    },
    onError: (error) => {
      notifications.show({
        color: 'red',
        title: t('common.error'),
        message: problemMessage(t, error),
      });
    },
  });
  const data = query.data;
  const cluster = bucket === undefined;
  const title = cluster ? t('blockPublic.clusterTitle') : t('blockPublic.bucketTitle');

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap" gap="xs">
          <Group gap="xs">
            <IconShieldLock size={20} aria-hidden="true" />
            <Title order={4}>{title}</Title>
          </Group>
          {data !== undefined ? (
            <Badge color={data.effective ? 'grape' : 'gray'} variant="light">
              {data.effective ? t('blockPublic.active') : t('blockPublic.inactive')}
            </Badge>
          ) : null}
        </Group>
        <Text size="sm" c="dimmed">
          {cluster ? t('blockPublic.clusterDescription') : t('blockPublic.bucketDescription')}
        </Text>
        {query.isPending ? (
          <Skeleton height={40} />
        ) : query.isError ? (
          <Alert color="red" variant="light" role="alert">
            {problemMessage(t, query.error)}
          </Alert>
        ) : data !== undefined ? (
          <Stack gap="xs">
            <PermissionGuard allowed={canEdit} action={StorageActions.manageAccessRules}>
              {(disabled) => (
                <Switch
                  label={cluster ? t('blockPublic.clusterSwitch') : t('blockPublic.bucketSwitch')}
                  checked={data.enabled}
                  disabled={disabled || mutation.isPending}
                  onChange={(event) => {
                    mutation.mutate({ enabled: event.currentTarget.checked });
                  }}
                />
              )}
            </PermissionGuard>
            {cluster ? (
              <Stack gap={4}>
                <Text size="sm" fw={500} id="block-public-mode">
                  {t('blockPublic.mode')}
                </Text>
                <SegmentedControl
                  aria-labelledby="block-public-mode"
                  value={data.mode}
                  disabled={!canEdit || mutation.isPending}
                  data={[
                    { value: 'reject', label: t('blockPublic.modes.reject') },
                    { value: 'ignore', label: t('blockPublic.modes.ignore') },
                  ]}
                  onChange={(value) => {
                    mutation.mutate({
                      enabled: data.enabled,
                      mode: value === 'ignore' ? 'ignore' : 'reject',
                    });
                  }}
                  style={{ alignSelf: 'flex-start' }}
                />
                <Text size="xs" c="dimmed">
                  {t(`blockPublic.modeHelp.${data.mode}`)}
                </Text>
              </Stack>
            ) : data.cluster ? (
              <Alert color="grape" variant="light">
                {t('blockPublic.clusterOn')}
              </Alert>
            ) : null}
          </Stack>
        ) : null}
      </Stack>
    </Paper>
  );
}
