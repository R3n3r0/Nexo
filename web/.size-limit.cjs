// Bundle budget of the console (docs/06 §6.5): the initial bundle, every
// script and stylesheet index.html loads, stays within 300 KiB gzip. The
// pages, the date pickers, the editor, the charts, the Italian strings and
// the demo mocks are lazy chunks outside the budget. Run after the build.
const { readFileSync } = require('node:fs');
const { join } = require('node:path');

const html = readFileSync(join(__dirname, 'dist', 'index.html'), 'utf8');
const initial = [...html.matchAll(/(?:src|href)="\/(assets\/[^"]+\.(?:js|css))"/g)].map((match) =>
  join('dist', match[1]),
);
if (initial.length === 0) {
  throw new Error('size-limit: no script or stylesheet in dist/index.html: build first');
}

module.exports = [
  {
    name: 'initial bundle (scripts and styles of index.html)',
    path: initial,
    limit: '300 KiB',
    gzip: true,
  },
];
