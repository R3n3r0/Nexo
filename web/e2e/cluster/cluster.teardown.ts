import { rm } from 'node:fs/promises';

import { test as teardown } from '@playwright/test';

import { modeStateFile, readState } from '../support/env';
import { stopNexo } from '../support/nexo';

/** Teardown of the "cluster" project: stops every node by PID and removes the drives. */
teardown('stop the cluster', async () => {
  const state = readState(modeStateFile('distributed'));
  await Promise.all((state.nodes ?? [state]).map((n) => stopNexo(n.pid)));
  if (process.env.NEXO_E2E_KEEP === '1') {
    console.warn(`[e2e] kept ${state.workDir}`);
    return;
  }
  await rm(state.workDir, { recursive: true, force: true });
  await rm(modeStateFile('distributed'), { force: true });
});
