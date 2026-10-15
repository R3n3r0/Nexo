import { useQuery } from '@tanstack/react-query';

import { DistributionActions, usePermissions } from '@/auth/usePermissions';
import { distributionKeys } from '@/lib/accessQueries';
import { useServices, type Distribution } from '@/services';

/**
 * The distributions the user may list (admin:ManageDistributions), or
 * undefined: a deploy pipeline with admin:InvalidateCache alone names the
 * distribution it purges (TD-C-06).
 */
export function useDistributionChoices(): Distribution[] | undefined {
  const { distributions } = useServices();
  const perms = usePermissions();
  const canList = perms.can(DistributionActions.manage);
  const list = useQuery({
    queryKey: distributionKeys.list,
    queryFn: () => distributions.list(),
    enabled: canList,
  });
  return canList ? (list.data ?? []) : undefined;
}
