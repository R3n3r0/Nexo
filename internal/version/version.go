// Package version exposes the build information injected at link time with
// -ldflags "-X github.com/r3n3r0/nexo/internal/version.Version=...".
package version

import (
	"fmt"
	"runtime"
)

// Build information. These are the only package-level variables allowed to
// be mutated, and only by the linker.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Info is a snapshot of the build information.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build information of the running binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String renders the info on one line.
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s, %s)", i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}
