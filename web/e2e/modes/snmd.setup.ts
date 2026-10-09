import { mkdir, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';

import { test as setup } from '@playwright/test';

import { drivesDirectory } from '../support/cluster';
import { modeStateFile, nexoBinary } from '../support/env';
import { startNexo, stopNexo } from '../support/nexo';
import { seed } from '../support/seed';

/**
 * Global setup of the "snmd" project (TM-02): bin/nexo on one node with 4
 * drives (erasure coding), seeded like the single-drive server of the
 * global setup; the teardown project stops it.
 */
setup('start a single node with 4 drives (SNMD)', async () => {
  const binary = nexoBinary();
  const workDir = await drivesDirectory('nexo-e2e-snmd-');
  const server = await startNexo({ binary, workDir, drives: 4 });
  try {
    const state = await seed(server, 'snmd', workDir);
    const file = modeStateFile('snmd');
    await mkdir(dirname(file), { recursive: true });
    await writeFile(file, JSON.stringify(state, null, 2));
    console.warn(`[e2e] snmd nexo pid ${String(server.pid)}: console ${server.consoleURL}`);
  } catch (error) {
    await stopNexo(server.pid);
    throw error;
  }
});
