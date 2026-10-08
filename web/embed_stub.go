//go:build !embedui

// Package web embeds the compiled React console into the server binary when
// built with the "embedui" tag. This file is the stub used without the tag
// (backend development): no assets are embedded.
package web

import "io/fs"

// Assets reports that the console is not included in this build.
func Assets() (fs.FS, bool) {
	return nil, false
}
