import type { HttpClient, QueryParams } from './client';

/** A page of a list endpoint ({items, nextCursor}). */
export interface Page<T> {
  items: T[];
  nextCursor?: string;
}

/** Upper bound of pages followed by collectPages (1000 items each). */
export const maxCollectedPages = 50;

/**
 * Fetches every page of a list endpoint by following nextCursor, up to
 * maxCollectedPages pages. The console lists IAM entities and share links
 * client-side (search, filters), and these collections stay small.
 */
export async function collectPages<T>(
  client: HttpClient,
  path: string,
  query: QueryParams = {},
): Promise<T[]> {
  const items: T[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < maxCollectedPages; page += 1) {
    const result = await client.request<Page<T>>(path, { query: { ...query, cursor } });
    items.push(...result.items);
    cursor = result.nextCursor;
    if (cursor === undefined || cursor === '') {
      break;
    }
  }
  return items;
}

/** Encodes a path segment (user names, policy names, ids). */
export const segment = (value: string): string => encodeURIComponent(value);
