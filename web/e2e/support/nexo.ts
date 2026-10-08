import { spawn } from 'node:child_process';
import { openSync, closeSync, readFileSync } from 'node:fs';
import { mkdir } from 'node:fs/promises';
import { request as httpRequest } from 'node:http';
import { request as httpsRequest } from 'node:https';
import { createServer } from 'node:net';
import { join } from 'node:path';

/** Root credentials of the servers started by the suite. */
export const rootCredentials = {
  accessKey: 'e2e-root-admin',
  secretKey: 'e2e-root-secret-0123456789',
} as const;

/** A running nexo server process. */
export interface NexoProcess {
  pid: number;
  /** Console + Admin API origin, e.g. http://127.0.0.1:41001. */
  consoleURL: string;
  /** S3 API origin, e.g. http://127.0.0.1:41000. */
  s3URL: string;
  dataDir: string;
  logFile: string;
  region: string;
}

export interface StartOptions {
  binary: string;
  /** Directory holding the drive and the log file. */
  workDir: string;
  /** Directory with public.crt/private.key: serves both listeners over TLS. */
  certsDir?: string;
  env?: Record<string, string>;
}

/** Asks the kernel for a free TCP port on the loopback interface. */
export function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.unref();
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      server.close(() => {
        resolve(port);
      });
    });
  });
}

/** GET a URL ignoring certificate errors; resolves to the status code (0 on failure). */
function probe(url: string): Promise<number> {
  return new Promise((resolve) => {
    const send = url.startsWith('https:') ? httpsRequest : httpRequest;
    const req = send(url, { method: 'GET', rejectUnauthorized: false, timeout: 2_000 }, (res) => {
      res.resume();
      resolve(res.statusCode ?? 0);
    });
    req.on('error', () => {
      resolve(0);
    });
    req.on('timeout', () => {
      req.destroy();
      resolve(0);
    });
    req.end();
  });
}

function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

const sleep = (ms: number) =>
  new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });

/**
 * Starts `nexo server` on two free loopback ports with a fresh drive under
 * workDir, waits until it is ready and returns its coordinates. The process
 * runs detached in its own process group and is stopped by PID (stopNexo).
 */
export async function startNexo(options: StartOptions): Promise<NexoProcess> {
  const dataDir = join(options.workDir, 'data');
  await mkdir(dataDir, { recursive: true });
  const logFile = join(options.workDir, 'nexo.log');
  const [s3Port, consolePort] = [await freePort(), await freePort()];
  const scheme = options.certsDir === undefined ? 'http' : 'https';
  const args = [
    'server',
    '--address',
    `127.0.0.1:${s3Port}`,
    '--console-address',
    `127.0.0.1:${consolePort}`,
    ...(options.certsDir === undefined ? [] : ['--certs-dir', options.certsDir]),
    dataDir,
  ];
  const log = openSync(logFile, 'a');
  const child = spawn(options.binary, args, {
    detached: true,
    stdio: ['ignore', log, log],
    env: {
      ...process.env,
      NEXO_ROOT_USER: rootCredentials.accessKey,
      NEXO_ROOT_PASSWORD: rootCredentials.secretKey,
      NEXO_LOG_FORMAT: 'text',
      NEXO_LOG_LEVEL: 'info',
      ...options.env,
    },
  });
  closeSync(log);
  child.unref();
  const pid = child.pid;
  if (pid === undefined) {
    throw new Error(`could not start ${options.binary}`);
  }
  const consoleURL = `${scheme}://127.0.0.1:${consolePort}`;
  const deadline = Date.now() + 30_000;
  for (;;) {
    if (!isAlive(pid)) {
      throw new Error(`nexo exited during startup:\n${readFileSync(logFile, 'utf8')}`);
    }
    if ((await probe(`${consoleURL}/nexo/health/ready`)) === 200) {
      break;
    }
    if (Date.now() > deadline) {
      await stopNexo(pid);
      throw new Error(`nexo was not ready within 30 s:\n${readFileSync(logFile, 'utf8')}`);
    }
    await sleep(100);
  }
  return {
    pid,
    consoleURL,
    s3URL: `${scheme}://127.0.0.1:${s3Port}`,
    dataDir,
    logFile,
    region: 'us-east-1',
  };
}

/** Stops a server by PID: SIGTERM, then SIGKILL after 10 s. */
export async function stopNexo(pid: number): Promise<void> {
  if (!isAlive(pid)) {
    return;
  }
  process.kill(pid, 'SIGTERM');
  const deadline = Date.now() + 10_000;
  while (isAlive(pid) && Date.now() < deadline) {
    await sleep(50);
  }
  if (isAlive(pid)) {
    process.kill(pid, 'SIGKILL');
  }
}
