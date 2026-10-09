import { join } from 'node:path';

import { request } from '@playwright/test';

import { NexoClient } from './api';
import type { E2EMode, E2EState } from './env';
import { type NexoProcess, rootCredentials } from './nexo';

/** Users created for the suite. */
export const member = { accessKey: 'e2e-member', secretKey: 'e2e-member-secret-01' };

/**
 * Seeds the test identities of a server through the Admin API (SigV4 as
 * root) and saves a root console session: the state of a deployment
 * ready for the specs.
 */
export async function seed(
  server: NexoProcess,
  mode: E2EMode,
  workDir: string,
  nodes?: NexoProcess[],
): Promise<E2EState> {
  const api = new NexoClient(server);
  // A user with every S3 permission and no admin:* action.
  await api.call('POST', '/users', { json: { ...member, policyNames: ['readwrite'] } });

  const rootStorageState = join(workDir, 'root-session.json');
  const ctx = await request.newContext({ baseURL: server.consoleURL });
  try {
    const res = await ctx.post('/api/v1/auth/login', { data: rootCredentials });
    if (!res.ok()) {
      throw new Error(`root login failed: ${String(res.status())} ${await res.text()}`);
    }
    await ctx.storageState({ path: rootStorageState });
  } finally {
    await ctx.dispose();
  }
  return {
    ...server,
    mode,
    ...(nodes === undefined ? {} : { nodes }),
    workDir,
    rootStorageState,
    users: { member },
  };
}
