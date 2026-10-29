// Package jssdk embeds the browser files of the JavaScript SDK of Nexo
// (sdk/js/src), which the S3 listener serves under /nexo/media/sdk/ for the
// pages that load it without a bundler.
package jssdk

import "embed"

// Files are the browser ES modules of the SDK, in src/ (the server side,
// server.js, is not served).
//
//go:embed src/index.js src/player.js src/element.js src/token.js
var Files embed.FS
