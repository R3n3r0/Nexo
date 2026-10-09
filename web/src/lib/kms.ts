import { useQuery } from '@tanstack/react-query';

import { isApiError, useServices } from '@/services';

import { storageKeys } from './storageKeys';

/**
 * The KMS as the user may see it: its status and keys with
 * admin:KMSKeyStatus, unknown otherwise (the server still refuses
 * SSE-S3/SSE-KMS without a KMS).
 */
export function useKms(enabled: boolean) {
  const { kms } = useServices();
  const status = useQuery({
    queryKey: storageKeys.kmsStatus,
    queryFn: () => kms.status(),
    enabled,
    retry: false,
    staleTime: 60_000,
  });
  const configured = status.data?.configured;
  const keys = useQuery({
    queryKey: storageKeys.kmsKeys,
    queryFn: () => kms.keys(),
    enabled: enabled && configured === true,
    retry: false,
    staleTime: 60_000,
  });
  // A server without KMS service answers 501: no SSE-S3/SSE-KMS either.
  const missing =
    configured === false ||
    (status.isError && isApiError(status.error) && status.error.status === 501);
  return {
    /** True/false when known, undefined when the user may not see the KMS. */
    configured: missing ? false : configured,
    keys: keys.data?.map((k) => k.id) ?? [],
    defaultKeyId: status.data?.defaultKeyId,
  };
}
