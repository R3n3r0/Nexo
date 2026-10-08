import { QueryClient } from '@tanstack/react-query';

import { isApiError } from '@/services';

/** Creates the TanStack Query client with the console defaults. */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        refetchOnWindowFocus: false,
        retry(failureCount, error) {
          // Never retry authentication or client errors; retry transient ones twice.
          if (isApiError(error) && error.status > 0 && error.status < 500) {
            return false;
          }
          return failureCount < 2;
        },
      },
      mutations: { retry: false },
    },
  });
}
