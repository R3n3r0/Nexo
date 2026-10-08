import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import type { NexoProcess } from './nexo';

/** Root of the repository (web/e2e/support → ../../..). */
export const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');

/** Environment variable naming the state file written by the global setup. */
export const stateVariable = 'NEXO_E2E_STATE';

/** Everything the specs need to reach the server under test. */
export interface E2EState extends NexoProcess {
  /** Temporary directory holding the drive, the log and the fixtures. */
  workDir: string;
  /** Storage state (cookies) of a root console session. */
  rootStorageState: string;
  /** Users seeded by the global setup. */
  users: {
    /** User with every S3 permission (readwrite) and no admin:* action. */
    member: { accessKey: string; secretKey: string };
  };
}

/** The state of the running suite (written by global-setup.ts). */
export function readState(): E2EState {
  const file = process.env[stateVariable];
  if (file === undefined || file === '') {
    throw new Error(`${stateVariable} is not set: run the suite with "npm run e2e"`);
  }
  return JSON.parse(readFileSync(file, 'utf8')) as E2EState;
}
