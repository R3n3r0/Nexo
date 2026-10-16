package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSource(t *testing.T) {
	cases := []struct{ version, commit, want string }{
		{"v1.0.0", "abc1234", SourceURL + "/tree/v1.0.0"},
		{"v1.0.0-rc.1", "abc1234", SourceURL + "/tree/v1.0.0-rc.1"},
		{"v1.0.0-3-gabc1234-dirty", "abc1234", SourceURL + "/tree/abc1234"},
		{"dev", "none", SourceURL},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Info{Version: c.version, Commit: c.commit}.sourceURL(), c.version)
	}
}

func TestLicenseLine(t *testing.T) {
	line := Info{Version: "dev", Commit: "none"}.LicenseLine()
	assert.Equal(t, "License AGPL-3.0-only (additional terms in NOTICE). Powered by Nexo; source code: https://github.com/R3n3r0/Nexo", line)
}
