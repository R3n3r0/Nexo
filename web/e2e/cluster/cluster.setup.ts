import { mkdir, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';

import { test as setup } from '@playwright/test';

import { startCluster } from '../support/cluster';
import { modeStateFile, nexoBinary } from '../support/env';
import { stopNexo } from '../support/nexo';
import { seed } from '../support/seed';

/** Nodes and drives per node of the cluster of the "cluster" project. */
export const clusterNodes = 4;
export const clusterDrivesPerNode = 2;

/**
 * Global setup of the "cluster" project (docs/06 §6.5, TD-E-15): a
 * cluster of 4 bin/nexo processes of 2 drives each on 127.0.0.1, seeded
 * through the first node, whose console the pages open; the teardown
 * project stops every process.
 */
setup('start a cluster of 4 nodes x 2 drives', async () => {
  setup.setTimeout(150_000);
  const { workDir, nodes } = await startCluster({
    binary: nexoBinary(),
    nodes: clusterNodes,
    drivesPerNode: clusterDrivesPerNode,
  });
  const first = nodes[0];
  try {
    if (first === undefined) {
      throw new Error('cluster without nodes');
    }
    const state = await seed(first, 'distributed', workDir, nodes);
    const file = modeStateFile('distributed');
    await mkdir(dirname(file), { recursive: true });
    await writeFile(file, JSON.stringify(state, null, 2));
    console.warn(
      `[e2e] cluster pids ${nodes.map((n) => String(n.pid)).join(', ')}: console ${first.consoleURL}`,
    );
  } catch (error) {
    await Promise.all(nodes.map((n) => stopNexo(n.pid)));
    throw error;
  }
});
