import { useInfiniteQuery } from '@tanstack/react-query';

import { storageKeys } from '@/lib/storageKeys';
import { useServices } from '@/services';

/** Page size of the folder view (the server maximum). */
export const listingPageSize = 1000;

/** The folder view of a prefix, page by page (cursor pagination). */
export function useObjectListing(bucket: string, prefix: string) {
  const { objects } = useServices();
  return useInfiniteQuery({
    queryKey: storageKeys.listing(bucket, prefix),
    queryFn: ({ pageParam }) =>
      objects.list(bucket, {
        prefix,
        limit: listingPageSize,
        ...(pageParam !== '' ? { cursor: pageParam } : {}),
      }),
    initialPageParam: '',
    getNextPageParam: (last) =>
      last.isTruncated && last.nextCursor !== undefined && last.nextCursor !== ''
        ? last.nextCursor
        : undefined,
  });
}
