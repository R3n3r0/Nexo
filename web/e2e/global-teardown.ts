import { rm } from 'node:fs/promises';

import { readState } from './support/env';
import { stopNexo } from './support/nexo';

/**
 * Stops the server by PID and removes its temporary directory (kept when
 * NEXO_E2E_KEEP=1, to inspect the drive and nexo.log after a failure).
 */
export default async function globalTeardown(): Promise<void> {
  const state = readState();
  await stopNexo(state.pid);
  if (process.env.NEXO_E2E_KEEP === '1') {
    console.warn(`[e2e] kept ${state.workDir}`);
    return;
  }
  await rm(state.workDir, { recursive: true, force: true });
}
