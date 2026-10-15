import { useQuery } from '@tanstack/react-query';

import { usePermissions } from '@/auth/usePermissions';
import { serverInfoQueryKey } from '@/lib/serverInfo';
import { useServices } from '@/services';

/**
 * The names the monitoring endpoints give the nodes ("host:port" in a
 * cluster, "local" on a single node), from GET /info when the user may
 * read it (admin:ServerInfo); empty otherwise.
 */
export function useNodeNames(): string[] {
  const { info } = useServices();
  const perms = usePermissions();
  const query = useQuery({
    queryKey: serverInfoQueryKey,
    queryFn: () => info.serverInfo(),
    enabled: perms.can('admin:ServerInfo'),
    staleTime: 60_000,
  });
  const data = query.data;
  if (data === undefined) return [];
  return data.mode === 'distributed' ? data.nodes.map((n) => n.endpoint) : ['local'];
}
