//go:build embedui

// Package web embeds the compiled React console (the web/dist directory
// produced by "npm run build") into the server binary. The embedding is
// enabled by the "embedui" build tag; without it the stub in embed_stub.go
// reports that no assets are available and the console transport serves a
// "console not included" page instead.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the console file system rooted at the build output
// directory (index.html at its root) and true. It never returns false in
// this build configuration: the dist directory is checked at compile time.
func Assets() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
