import {
  ActionIcon,
  Anchor,
  Button,
  CopyButton,
  Group,
  Skeleton,
  Stack,
  Table,
  Text,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import { IconCheck, IconCopy } from '@tabler/icons-react';
import { useInfiniteQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { ErrorAlert } from '@/components/iam/common';
import { formatBytes } from '@/lib/format';
import { iamKeys } from '@/lib/iamQueries';
import { useServices } from '@/services';

/** Links fetched per page of a shared prefix. */
const linksPageSize = 100;

/**
 * The links of a shared prefix (D-SHR-01): one download link per object,
 * all signed by the same managed share and revoked with it, as
 * `nexoctl share download --recursive`. Paginated, with a copy button per
 * link and one for every link loaded.
 */
export function ShareLinks({ shareId }: { shareId: string }) {
  const { t, i18n } = useTranslation();
  const { shares } = useServices();
  const query = useInfiniteQuery({
    queryKey: [...iamKeys.shares, shareId, 'links'],
    queryFn: ({ pageParam }) =>
      shares.links(
        shareId,
        pageParam !== '' ? { cursor: pageParam, limit: linksPageSize } : { limit: linksPageSize },
      ),
    initialPageParam: '',
    getNextPageParam: (last) =>
      last.nextCursor !== undefined && last.nextCursor !== '' ? last.nextCursor : undefined,
  });
  if (query.isPending) {
    return <Skeleton height={80} />;
  }
  if (query.isError) {
    return <ErrorAlert error={query.error} />;
  }
  const links = query.data.pages.flatMap((page) => page.items);
  if (links.length === 0 && !query.hasNextPage) {
    return (
      <Text size="sm" c="dimmed">
        {t('shareDialog.links.empty')}
      </Text>
    );
  }
  const all = links.map((link) => link.url).join('\n');
  return (
    <Stack gap="xs">
      <Group justify="space-between" align="center" wrap="wrap" gap="xs">
        <Text size="sm">{t('shareDialog.links.hint', { count: links.length })}</Text>
        <CopyButton value={all}>
          {({ copied, copy }) => (
            <Button
              size="xs"
              variant="default"
              leftSection={copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
              onClick={copy}
            >
              {copied ? t('common.copied') : t('shareDialog.links.copyAll')}
            </Button>
          )}
        </CopyButton>
      </Group>
      <Table.ScrollContainer minWidth={320} mah={320}>
        <Table aria-label={t('shareDialog.links.label')} verticalSpacing={4}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('shareDialog.links.object')}</Table.Th>
              <Table.Th>{t('shareDialog.links.size')}</Table.Th>
              <Table.Th>
                <VisuallyHidden>{t('shareDialog.links.copy')}</VisuallyHidden>
              </Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {links.map((link) => (
              <Table.Tr key={link.key}>
                <Table.Td style={{ overflowWrap: 'anywhere' }}>
                  <Anchor href={link.url} target="_blank" rel="noreferrer" size="sm">
                    {link.key}
                  </Anchor>
                </Table.Td>
                <Table.Td>
                  <Text size="sm" c="dimmed">
                    {formatBytes(link.size, i18n.resolvedLanguage)}
                  </Text>
                </Table.Td>
                <Table.Td>
                  <CopyButton value={link.url}>
                    {({ copied, copy }) => (
                      <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
                        <ActionIcon
                          variant="subtle"
                          aria-label={t('shareDialog.links.copyOne', { key: link.key })}
                          onClick={copy}
                        >
                          {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
                        </ActionIcon>
                      </Tooltip>
                    )}
                  </CopyButton>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      {query.hasNextPage ? (
        <Button
          size="xs"
          variant="subtle"
          loading={query.isFetchingNextPage}
          onClick={() => {
            void query.fetchNextPage();
          }}
        >
          {t('shareDialog.links.more')}
        </Button>
      ) : null}
    </Stack>
  );
}
