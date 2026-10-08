import { execFileSync } from 'node:child_process';
import { existsSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { request } from '@playwright/test';

import { NexoClient } from './support/api';
import { type E2EState, repoRoot, stateVariable } from './support/env';
import { rootCredentials, startNexo, stopNexo } from './support/nexo';

/** Sources the binary embeds: a newer file means bin/nexo is stale. */
const sources = ['cmd', 'internal', 'pkg', 'api', 'go.mod', 'go.sum', 'web/src', 'web/index.html'];

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
function ensureBinary(): string {
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

/** Users created for the suite. */
const member = { accessKey: 'e2e-member', secretKey: 'e2e-member-secret-01' };

/**
 * Starts bin/nexo on free ports with a temporary drive and root
 * credentials, seeds the test identities through the Admin API (SigV4 as
 * root), saves a root console session and publishes everything in the
 * state file named by $NEXO_E2E_STATE.
 */
export default async function globalSetup(): Promise<void> {
  const binary = ensureBinary();
  const workDir = await mkdtemp(join(tmpdir(), 'nexo-e2e-'));
  const server = await startNexo({ binary, workDir });
  try {
    const api = new NexoClient(server);
    // A user with every S3 permission and no admin:* action.
    await api.call('POST', '/users', {
      json: { ...member, policyNames: ['readwrite'] },
    });

    const rootStorageState = join(workDir, 'root-session.json');
    const ctx = await request.newContext({ baseURL: server.consoleURL });
    const res = await ctx.post('/api/v1/auth/login', { data: rootCredentials });
    if (!res.ok()) {
      throw new Error(`root login failed: ${res.status()} ${await res.text()}`);
    }
    await ctx.storageState({ path: rootStorageState });
    await ctx.dispose();

    const state: E2EState = {
      ...server,
      workDir,
      rootStorageState,
      users: { member },
    };
    const file = join(workDir, 'state.json');
    writeFileSync(file, JSON.stringify(state, null, 2));
    process.env[stateVariable] = file;
    console.warn(`[e2e] nexo pid ${server.pid}: console ${server.consoleURL}, s3 ${server.s3URL}`);
  } catch (error) {
    await stopNexo(server.pid);
    throw error;
  }
}
