import { rm } from 'node:fs/promises';

import { test as teardown } from '@playwright/test';

import { modeStateFile, readState } from '../support/env';
import { stopNexo } from '../support/nexo';

/** Teardown of the "snmd" project: stops the server and removes its drives. */
teardown('stop the SNMD server', async () => {
  const state = readState(modeStateFile('snmd'));
  await stopNexo(state.pid);
  if (process.env.NEXO_E2E_KEEP === '1') {
    console.warn(`[e2e] kept ${state.workDir}`);
    return;
  }
  await rm(state.workDir, { recursive: true, force: true });
  await rm(modeStateFile('snmd'), { force: true });
});
