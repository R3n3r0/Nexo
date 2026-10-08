import {
  Alert,
  Anchor,
  Badge,
  Group,
  Paper,
  SimpleGrid,
  Skeleton,
  Stack,
  Text,
  Title,
} from '@mantine/core';
import { IconBucket } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { formatBytes, formatNumber } from '@/lib/format';
import { browsePath } from '@/lib/objectPath';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices } from '@/services';

/** Entry point of the object browser: pick a bucket. */
export function ObjectsPage() {
  const { t, i18n } = useTranslation();
  const { buckets } = useServices();
  const query = useQuery({ queryKey: storageKeys.buckets, queryFn: () => buckets.list() });
  const locale = i18n.resolvedLanguage;

  return (
    <Stack gap="lg">
      <Stack gap={2}>
        <Title order={2}>{t('objects.title')}</Title>
        <Text c="dimmed" size="sm">
          {t('objects.pickBucket')}
        </Text>
      </Stack>
      {query.isError ? (
        <Alert color="red" variant="light" role="alert">
          {problemMessage(t, query.error)}
        </Alert>
      ) : null}
      <SimpleGrid cols={{ base: 1, xs: 2, lg: 3 }} spacing="md">
        {query.isPending
          ? [0, 1, 2].map((i) => <Skeleton key={i} height={88} radius="md" />)
          : (query.data ?? []).map((bucket) => (
              <Paper key={bucket.name} withBorder radius="md" p="md">
                <Stack gap={4}>
                  <Group gap="xs" wrap="nowrap">
                    <IconBucket size={18} aria-hidden />
                    <Anchor component={Link} to={browsePath(bucket.name)} fw={600} truncate="end">
                      {bucket.name}
                    </Anchor>
                    {bucket.public === true ? (
                      <Badge size="xs" color="orange" variant="light">
                        {t('buckets.public')}
                      </Badge>
                    ) : null}
                  </Group>
                  <Text size="sm" c="dimmed">
                    {t('bucket.summary', {
                      size: formatBytes(bucket.size, locale),
                      objects: formatNumber(bucket.objects, locale),
                    })}
                  </Text>
                </Stack>
              </Paper>
            ))}
      </SimpleGrid>
      {query.isSuccess && query.data.length === 0 ? <Text>{t('buckets.empty')}</Text> : null}
    </Stack>
  );
}
