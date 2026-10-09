import { Alert, Button, Skeleton, Stack, Table, Text } from '@mantine/core';
import { useInfiniteQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import { formatBytes } from '@/lib/format';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type ObjectChecksum } from '@/services';

/** Parts per page of the attributes. */
const pageSize = 100;

function ChecksumText({ checksum }: { checksum: ObjectChecksum | undefined }) {
  const { t } = useTranslation();
  if (checksum === undefined) {
    return <>{t('objectAttributes.noChecksum')}</>;
  }
  return (
    <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
      {checksum.algorithm}
      {checksum.type !== undefined ? ` (${checksum.type})` : ''} {checksum.value}
    </Text>
  );
}

/** D-OBJ-06: checksum and parts of a version (S3 GetObjectAttributes). */
export function ObjectAttributesPanel({
  bucket,
  objectKey,
  versionId,
}: {
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
}) {
  const { t, i18n } = useTranslation();
  const { objects } = useServices();
  const query = useInfiniteQuery({
    queryKey: storageKeys.attributes(bucket, objectKey, versionId),
    queryFn: ({ pageParam }) =>
      objects.attributes(bucket, objectKey, {
        ...(versionId !== undefined ? { versionId } : {}),
        maxParts: pageSize,
        ...(pageParam > 0 ? { partNumberMarker: pageParam } : {}),
      }),
    initialPageParam: 0,
    getNextPageParam: (last) =>
      last.isTruncated && last.nextPartNumberMarker !== undefined
        ? last.nextPartNumberMarker
        : undefined,
  });
  const locale = i18n.resolvedLanguage;
  if (query.isPending) {
    return <Skeleton height={100} />;
  }
  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {problemMessage(t, query.error)}
      </Alert>
    );
  }
  const first = query.data.pages[0];
  const parts = query.data.pages.flatMap((p) => p.parts);
  return (
    <Stack gap="sm">
      <Table variant="vertical" layout="fixed" withTableBorder>
        <Table.Tbody>
          <Table.Tr>
            <Table.Th w={150}>{t('objectAttributes.checksum')}</Table.Th>
            <Table.Td>
              <ChecksumText checksum={first?.checksum} />
            </Table.Td>
          </Table.Tr>
          <Table.Tr>
            <Table.Th w={150}>{t('objectAttributes.parts')}</Table.Th>
            <Table.Td>
              {first === undefined || first.partsCount === 0
                ? t('objectAttributes.singlePart')
                : first.partsCount}
            </Table.Td>
          </Table.Tr>
        </Table.Tbody>
      </Table>
      {parts.length > 0 ? (
        <Table aria-label={t('objectAttributes.partsTable')} verticalSpacing={4}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('objectAttributes.partNumber')}</Table.Th>
              <Table.Th>{t('objectAttributes.partSize')}</Table.Th>
              <Table.Th>{t('objectAttributes.checksum')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {parts.map((part) => (
              <Table.Tr key={part.partNumber}>
                <Table.Td>{part.partNumber}</Table.Td>
                <Table.Td>{formatBytes(part.size, locale)}</Table.Td>
                <Table.Td>
                  <ChecksumText checksum={part.checksum} />
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      ) : null}
      {query.hasNextPage ? (
        <Button
          size="xs"
          variant="subtle"
          loading={query.isFetchingNextPage}
          onClick={() => {
            void query.fetchNextPage();
          }}
        >
          {t('objectAttributes.moreParts')}
        </Button>
      ) : null}
    </Stack>
  );
}
