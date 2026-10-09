import { execFile } from 'node:child_process';
import { mkdtemp } from 'node:fs/promises';
import { join } from 'node:path';

import type { E2EState } from './env';
import { nexoctlBinary } from './env';
import { rootCredentials } from './nexo';

/** The outcome of a nexoctl command. */
export interface CliResult {
  code: number;
  stdout: string;
  stderr: string;
}

function run(args: string[], configDir: string): Promise<CliResult> {
  return new Promise((resolve) => {
    execFile(
      nexoctlBinary(),
      args,
      {
        env: { ...process.env, NEXOCTL_CONFIG_DIR: configDir, NO_COLOR: '1' },
        timeout: 30_000,
      },
      (error, stdout, stderr) => {
        const code = error === null ? 0 : typeof error.code === 'number' ? error.code : 1;
        resolve({ code, stdout, stderr });
      },
    );
  });
}

/**
 * nexoctl against the server under test (TD-E-14: what the console stores
 * is visible from the CLI): a private configuration directory with the
 * alias "e2e" of root, S3 and Admin API.
 */
export class Nexoctl {
  private configDir: string | undefined;

  constructor(private readonly state: Pick<E2EState, 'consoleURL' | 's3URL' | 'workDir'>) {}

  private async dir(): Promise<string> {
    if (this.configDir === undefined) {
      const dir = await mkdtemp(join(this.state.workDir, 'nexoctl-'));
      const set = await run(
        [
          'alias',
          'set',
          'e2e',
          this.state.s3URL,
          rootCredentials.accessKey,
          rootCredentials.secretKey,
          '--admin-url',
          this.state.consoleURL,
        ],
        dir,
      );
      if (set.code !== 0) {
        throw new Error(`nexoctl alias set failed (${String(set.code)}): ${set.stderr}`);
      }
      this.configDir = dir;
    }
    return this.configDir;
  }

  /** Runs nexoctl with the arguments; "e2e/" names the alias of the server. */
  async run(...args: string[]): Promise<CliResult> {
    return run(args, await this.dir());
  }

  /** Runs nexoctl and fails on a non-zero exit code. */
  async ok(...args: string[]): Promise<string> {
    const result = await this.run(...args);
    if (result.code !== 0) {
      throw new Error(
        `nexoctl ${args.join(' ')} exited with ${String(result.code)}:\n${result.stderr}${result.stdout}`,
      );
    }
    return result.stdout;
  }
}
