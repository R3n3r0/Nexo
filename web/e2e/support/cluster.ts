import { statfsSync } from 'node:fs';
import { mkdir, mkdtemp } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { e2eKmsSecretKey } from './env';
import { freePort, type NexoProcess, spawnNexo, stopNexo, waitReady } from './nexo';

/**
 * Internode settings of the test clusters, so that a lost node resolves
 * in seconds: an internode call gives up after 5 s, orphan locks expire
 * after 6 s (refreshed every 2 s), and the background healing retries an
 * object at least every 5 s (its 20 attempts span about 90 s), so that a
 * node coming back is healed at once. Every node has the same builtin
 * KMS key of the SNSD server, so that the encryption specs run on the
 * cluster too (cluster-full).
 */
export const clusterEnv = {
  NEXO_RPC_CALL_TIMEOUT: '5s',
  NEXO_LOCK_STALE_AFTER: '6s',
  NEXO_LOCK_REFRESH_INTERVAL: '2s',
  NEXO_HEAL_RETRY_MAX_DELAY: '5s',
  NEXO_KMS_SECRET_KEY: e2eKmsSecretKey,
};

/**
 * A directory for the drives of the erasure-coded deployments: below
 * $NEXO_E2E_DIR when set, else on the tmpfs of Linux when it has room
 * (fast, and a nearly full disk under the temporary directory would make
 * the pools refuse the writes as full), else the temporary directory.
 */
export async function drivesDirectory(prefix: string): Promise<string> {
  let root = process.env.NEXO_E2E_DIR ?? '';
  if (root === '' && process.platform === 'linux' && roomy('/dev/shm')) {
    root = '/dev/shm';
  }
  return mkdtemp(join(root === '' ? tmpdir() : root, prefix));
}

/** True when the file system of path has at least 2 GiB free and is under 80% full. */
function roomy(path: string): boolean {
  try {
    const st = statfsSync(path);
    const free = st.bavail * st.bsize;
    const total = st.blocks * st.bsize;
    return free >= 2 * 1024 ** 3 && total > 0 && free / total > 0.2;
  } catch {
    return false;
  }
}

/** True when the TCP port can be bound on the loopback interface. */
function portFree(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const server = createServer();
    server.unref();
    server.once('error', () => {
      resolve(false);
    });
    server.listen(port, '127.0.0.1', () => {
      server.close(() => {
        resolve(true);
      });
    });
  });
}

/** n consecutive free ports (the ellipsis on the port names them all). */
async function consecutivePorts(n: number): Promise<number> {
  for (let attempt = 0; attempt < 50; attempt++) {
    const base = 20_000 + Math.floor(Math.random() * 30_000);
    let ok = true;
    for (let i = 0; i < n && ok; i++) {
      ok = await portFree(base + i);
    }
    if (ok) {
      return base;
    }
  }
  throw new Error(`no ${String(n)} consecutive free ports`);
}

export interface ClusterOptions {
  binary: string;
  nodes: number;
  drivesPerNode: number;
}

/**
 * Starts a cluster of bin/nexo processes on 127.0.0.1: node i listens on
 * consecutive S3 ports (the internode RPC too) and its own console port,
 * runs in workDir/n<i> and keeps its drives below. On Linux every node
 * gets the same command line, the ellipsis on the port and the
 * per-process /proc/self/cwd naming the drives of each:
 *
 *   http://127.0.0.1:{p...p+3}/proc/self/cwd/d{1...2}
 *
 * elsewhere every drive is listed. Resolves once every node is ready;
 * on failure the started nodes are stopped.
 */
export async function startCluster(
  options: ClusterOptions,
): Promise<{ workDir: string; nodes: NexoProcess[] }> {
  const workDir = await drivesDirectory('nexo-e2e-cluster-');
  const base = await consecutivePorts(options.nodes);
  const nodes: NexoProcess[] = [];
  for (let i = 0; i < options.nodes; i++) {
    const dir = join(workDir, `n${String(i + 1)}`);
    await mkdir(dir, { recursive: true });
    nodes.push({
      pid: 0,
      s3URL: `http://127.0.0.1:${String(base + i)}`,
      consoleURL: `http://127.0.0.1:${String(await freePort())}`,
      dataDir: dir,
      logFile: join(dir, 'nexo.log'),
      region: 'us-east-1',
    });
  }
  const drives = options.drivesPerNode;
  const endpoints =
    process.platform === 'linux'
      ? [
          `http://127.0.0.1:{${String(base)}...${String(base + options.nodes - 1)}}/proc/self/cwd/d{1...${String(drives)}}`,
        ]
      : nodes.flatMap((n) =>
          Array.from(
            { length: drives },
            (_, d) => `${n.s3URL}${n.dataDir.replaceAll('\\', '/')}/d${String(d + 1)}`,
          ),
        );
  try {
    for (const node of nodes) {
      node.pid = spawnNexo({
        binary: options.binary,
        cwd: node.dataDir,
        logFile: node.logFile,
        env: clusterEnv,
        args: [
          'server',
          '--address',
          node.s3URL.replace('http://', ''),
          '--console-address',
          node.consoleURL.replace('http://', ''),
          ...endpoints,
        ],
      });
    }
    for (const node of nodes) {
      await waitReady(node.pid, node.consoleURL, node.logFile, 90_000);
    }
  } catch (error) {
    await Promise.all(nodes.filter((n) => n.pid !== 0).map((n) => stopNexo(n.pid)));
    throw error;
  }
  return { workDir, nodes };
}
