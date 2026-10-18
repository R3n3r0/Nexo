import { execFileSync } from 'node:child_process';
import { existsSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { e2eKmsSecretKey, nexoctlBinary, repoRoot, stateVariable } from './support/env';
import { startNexo, stopNexo } from './support/nexo';
import { seed } from './support/seed';

/** Sources the binary embeds: a newer file means bin/nexo is stale. */
const sources = [
  'cmd',
  'internal',
  'pkg',
  'api',
  'go.mod',
  'go.sum',
  'web/src',
  'web/index.html',
  'web/player.html',
];

function newestMtime(path: string): number {
  const st = statSync(path, { throwIfNoEntry: false });
  if (st === undefined) {
    return 0;
  }
  if (!st.isDirectory()) {
    return st.mtimeMs;
  }
  let newest = st.mtimeMs;
  for (const entry of readdirSync(path)) {
    newest = Math.max(newest, newestMtime(join(path, entry)));
  }
  return newest;
}

/**
 * The binary under test: $NEXO_E2E_BINARY, or bin/nexo built with the
 * embedded dashboard (`make build`). When bin/nexo is missing or older than
 * its sources the dashboard and the binary are rebuilt (npm run build +
 * make build-embedded: unlike `make build` it does not run npm ci, which
 * would replace node_modules under the running Playwright).
 */
export function ensureBinary(): string {
  const explicit = process.env.NEXO_E2E_BINARY;
  if (explicit !== undefined && explicit !== '') {
    return explicit;
  }
  const binary = join(repoRoot, 'bin', 'nexo');
  const built = statSync(binary, { throwIfNoEntry: false })?.mtimeMs ?? 0;
  const newest = Math.max(...sources.map((s) => newestMtime(join(repoRoot, s))));
  if (built === 0 || newest > built) {
    console.warn(`[e2e] ${built === 0 ? 'building' : 'rebuilding stale'} bin/nexo`);
    execFileSync('npm', ['run', 'build'], { cwd: join(repoRoot, 'web'), stdio: 'inherit' });
    execFileSync('make', ['build-embedded'], { cwd: repoRoot, stdio: 'inherit' });
  }
  if (!existsSync(join(repoRoot, 'web', 'dist', 'index.html'))) {
    throw new Error('web/dist is missing: run make build');
  }
  return binary;
}

/** Go sources of nexoctl: a newer file means bin/nexoctl is stale. */
const cliSources = ['cmd', 'internal', 'pkg', 'go.mod', 'go.sum'];

/**
 * The CLI the specs check the console against (TD-E-14): $NEXO_E2E_NEXOCTL,
 * or bin/nexoctl, rebuilt when missing or older than its sources.
 */
export function ensureCli(): string {
  const binary = nexoctlBinary();
  if (process.env.NEXO_E2E_NEXOCTL) {
    return binary;
  }
  const built = statSync(binary, { throwIfNoEntry: false })?.mtimeMs ?? 0;
  const newest = Math.max(...cliSources.map((s) => newestMtime(join(repoRoot, s))));
  if (built === 0 || newest > built) {
    console.warn(`[e2e] ${built === 0 ? 'building' : 'rebuilding stale'} bin/nexoctl`);
    execFileSync('go', ['build', '-trimpath', '-o', binary, './cmd/nexoctl'], {
      cwd: repoRoot,
      stdio: 'inherit',
    });
  }
  return binary;
}

/**
 * Starts bin/nexo on free ports with a temporary drive, root credentials
 * and the builtin KMS, seeds the test identities through the Admin API (SigV4 as
 * root), saves a root console session and publishes everything in the
 * state file named by $NEXO_E2E_STATE.
 */
export default async function globalSetup(): Promise<void> {
  const binary = ensureBinary();
  ensureCli();
  const workDir = await mkdtemp(join(tmpdir(), 'nexo-e2e-'));
  // The builtin KMS serves SSE-S3 and SSE-KMS (D-OBJ-06, upload encryption).
  const server = await startNexo({
    binary,
    workDir,
    env: { NEXO_KMS_SECRET_KEY: e2eKmsSecretKey },
  });
  try {
    const state = await seed(server, 'snsd', workDir);
    const file = join(workDir, 'state.json');
    writeFileSync(file, JSON.stringify(state, null, 2));
    process.env[stateVariable] = file;
    console.warn(`[e2e] nexo pid ${server.pid}: console ${server.consoleURL}, s3 ${server.s3URL}`);
  } catch (error) {
    await stopNexo(server.pid);
    throw error;
  }
}
