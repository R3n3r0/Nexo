// Runs after `npm ci` / `npm install` (see "postinstall" in package.json).
//
// Some npm packages ship Go sources (for example flatted/golang). The Go
// toolchain walks every directory of the repository when a `./...` pattern is
// used, so those files would be compiled, vetted and linted together with the
// Nexo sources. Go skips any directory that contains its own go.mod, so we
// drop a placeholder module file into node_modules to fence it off. The
// directory is gitignored and recreated by npm, hence the file is generated
// here instead of being committed.
import { existsSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const webDir = dirname(dirname(fileURLToPath(import.meta.url)));
const nodeModules = join(webDir, 'node_modules');

if (existsSync(nodeModules)) {
  writeFileSync(
    join(nodeModules, 'go.mod'),
    'module nexo.invalid/web/node_modules\n\n// Placeholder: keeps the Go toolchain out of node_modules.\ngo 1.24\n',
  );
}
