import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import type { NexoProcess } from './nexo';

/** Root of the repository (web/e2e/support → ../../..). */
export const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');

/** Environment variable naming the state file written by the global setup. */
export const stateVariable = 'NEXO_E2E_STATE';

/** Deployment modes of the suite (docs/08 §8.6 TM-02). */
export type E2EMode = 'snsd' | 'snmd' | 'distributed';

/** The label the console shows for each mode (modes.* in the locales). */
export const modeLabels: Record<E2EMode, string> = {
  snsd: 'Single node, single drive',
  snmd: 'Single node, multiple drives',
  distributed: 'Distributed cluster',
};

/**
 * Everything the specs need to reach the server under test. In a cluster
 * the top-level coordinates are those of the first node, whose console
 * the pages open; nodes lists every process.
 */
export interface E2EState extends NexoProcess {
  mode: E2EMode;
  /** The processes of a cluster, in node order (absent on a single node). */
  nodes?: NexoProcess[];
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

/**
 * The state of the running suite: the file given by a project (the setup
 * of the SNMD and cluster projects), else the one of the global setup
 * named by $NEXO_E2E_STATE.
 */
export function readState(file?: string): E2EState {
  const path = file ?? process.env[stateVariable];
  if (path === undefined || path === '') {
    throw new Error(`${stateVariable} is not set: run the suite with "npm run e2e"`);
  }
  return JSON.parse(readFileSync(path, 'utf8')) as E2EState;
}

/**
 * Directory of the state files of the SNMD and cluster projects of this
 * run (shared by the runner and its workers through the environment).
 */
export function runDirectory(): string {
  const dir = process.env.NEXO_E2E_RUN_DIR;
  if (dir === undefined || dir === '') {
    throw new Error('NEXO_E2E_RUN_DIR is not set: run the suite with "npm run e2e"');
  }
  return dir;
}

/** The state file of a mode project. */
export function modeStateFile(mode: Exclude<E2EMode, 'snsd'>): string {
  return join(runDirectory(), `${mode}-state.json`);
}

/**
 * The binary under test: $NEXO_E2E_BINARY, or bin/nexo, which the global
 * setup rebuilt when it was stale.
 */
export function nexoBinary(): string {
  const explicit = process.env.NEXO_E2E_BINARY;
  return explicit !== undefined && explicit !== '' ? explicit : join(repoRoot, 'bin', 'nexo');
}
