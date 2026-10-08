/**
 * TanStack Query keys of the IAM, share and configuration pages, and the
 * mutation helper that refreshes them. Keys are hierarchical so that
 * invalidating ['users'] refreshes the list and every user detail.
 */
import {
  useMutation,
  useQueryClient,
  type QueryKey,
  type UseMutationOptions,
  type UseMutationResult,
} from '@tanstack/react-query';

import type { AccessKeyFilter, PermissionsQuery, ShareFilter } from '@/services';

export const iamKeys = {
  users: ['users'] as const,
  user: (accessKey: string) => ['users', accessKey] as const,
  groups: ['groups'] as const,
  group: (name: string) => ['groups', name] as const,
  policies: ['policies'] as const,
  policy: (name: string) => ['policies', name] as const,
  policyEntities: (name: string) => ['policies', name, 'entities'] as const,
  accessKeys: ['access-keys'] as const,
  accessKeyList: (filter: AccessKeyFilter = {}) =>
    ['access-keys', 'list', filter.user ?? ''] as const,
  shares: ['shares'] as const,
  shareList: (filter: ShareFilter = {}) =>
    ['shares', 'list', filter.owner ?? '', filter.bucket ?? ''] as const,
  config: ['config'] as const,
  configSchema: ['config', 'schema'] as const,
  permissions: ['permissions'] as const,
  permissionsOf: (query: PermissionsQuery = {}) =>
    ['permissions', query.user ?? '', query.bucket ?? '', query.prefix ?? ''] as const,
};

export interface InvalidatingMutationOptions<TData, TVariables> extends Omit<
  UseMutationOptions<TData, Error, TVariables>,
  'onSuccess'
> {
  /** Query keys refreshed after a success (prefix match). */
  invalidate: readonly QueryKey[];
  onSuccess?: (data: TData, variables: TVariables) => void | Promise<void>;
}

/**
 * useMutation that invalidates the given queries once the mutation
 * succeeds, before calling onSuccess, so that every list showing the
 * changed entity is refreshed (TD-U-14).
 */
export function useInvalidatingMutation<TData, TVariables = void>({
  invalidate,
  onSuccess,
  ...options
}: InvalidatingMutationOptions<TData, TVariables>): UseMutationResult<TData, Error, TVariables> {
  const queryClient = useQueryClient();
  return useMutation<TData, Error, TVariables>({
    ...options,
    async onSuccess(data, variables) {
      await Promise.all(invalidate.map((queryKey) => queryClient.invalidateQueries({ queryKey })));
      await onSuccess?.(data, variables);
    },
  });
}
