// Package version exposes the build information injected at link time with
// -ldflags "-X github.com/r3n3r0/nexo/internal/version.Version=...".
package version

import (
	"fmt"
	"regexp"
	"runtime"
)

// License and source of Nexo (AGPL-3.0 sections 5(d) and 13: the
// Appropriate Legal Notices and the offer of the source code).
const (
	// License is the SPDX identifier of the license of Nexo.
	License = "AGPL-3.0-only"
	// SourceURL is the repository of the source code of Nexo.
	SourceURL = "https://github.com/R3n3r0/Nexo"
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
	License   string `json:"license"`
	Source    string `json:"source"`
}

// Get returns the build information of the running binary.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		License:   License,
	}
	info.Source = info.sourceURL()
	return info
}

// String renders the info on one line.
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s, %s)", i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}

var (
	releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
	commitHash = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// sourceURL returns the URL of the source code of this build: the tree of its
// release tag, else of its commit, else the repository.
func (i Info) sourceURL() string {
	switch {
	case releaseTag.MatchString(i.Version):
		return SourceURL + "/tree/" + i.Version
	case commitHash.MatchString(i.Commit):
		return SourceURL + "/tree/" + i.Commit
	default:
		return SourceURL
	}
}

// LicenseLine is the license notice printed by --version and the version
// commands of nexo and nexoctl.
func (i Info) LicenseLine() string {
	return fmt.Sprintf("License %s (additional terms in NOTICE). Powered by Nexo; source code: %s", License, i.sourceURL())
}
