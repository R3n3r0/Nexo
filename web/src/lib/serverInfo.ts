import type { ServerInfo } from '@/services';

/** Query key of the server information. */
export const serverInfoQueryKey = ['info', 'server'] as const;

export interface InfoSummary {
  nodes: number;
  nodesOnline: number;
  drives: number;
  drivesOnline: number;
}

/** Counts nodes and drives and how many of them are online. */
export function summariseServerInfo(info: ServerInfo): InfoSummary {
  let drives = 0;
  let drivesOnline = 0;
  for (const node of info.nodes) {
    for (const disk of node.disks) {
      drives += 1;
      if (disk.state === 'online') {
        drivesOnline += 1;
      }
    }
  }
  return {
    nodes: info.nodes.length,
    nodesOnline: info.nodes.filter((node) => node.state === 'online').length,
    drives,
    drivesOnline,
  };
}
