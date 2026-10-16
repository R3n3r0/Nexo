/**
 * License and source of Nexo, shown by the console to every user (AGPL-3.0
 * §5(d) Appropriate Legal Notices, §13 offer of the source code, NOTICE
 * §7(b) attribution). Mirrors internal/version of the server.
 */

/** SPDX identifier of the license of Nexo. */
export const license = 'AGPL-3.0-only';

/** Repository of the source code of Nexo. */
export const sourceRepository = 'https://github.com/R3n3r0/Nexo';

/** The text of the license. */
export const licenseUrl = 'https://www.gnu.org/licenses/agpl-3.0.html';

const releaseTag = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/;
const commitHash = /^[0-9a-f]{7,40}$/;

/**
 * The source code of a build: the tree of its release tag, else of its
 * commit, else the repository.
 */
export function sourceUrl(version?: string, commit?: string): string {
  if (version !== undefined && releaseTag.test(version)) {
    return `${sourceRepository}/tree/${version}`;
  }
  if (commit !== undefined && commitHash.test(commit)) {
    return `${sourceRepository}/tree/${commit}`;
  }
  return sourceRepository;
}
