package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repo writes a minimal repository: a catalog with one P0 and one P2
// feature, the documents of the detailed codes and a test tagged with
// tests (the tag is built to keep it out of this file).
func repo(t *testing.T, tests string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"docs/02-features.md":                     "## 2.1 Server\n| S-OBJ-01 | Put | P0 |\n| S-OBJ-14 | S2 | P2 |\n",
		"docs/10-domains-access-distributions.md": "",
		"docs/11-domain-cache.md":                 "",
		"test/x_test.go":                          "package x\n\n// " + "cov" + "ers: " + tests + "\nfunc TestX(t *testing.T) {}\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return root
}

func TestRun(t *testing.T) {
	tests := []struct {
		name   string
		tags   string
		first  []string
		code   int
		stderr string
	}{
		{name: "write then check", tags: "S-OBJ-01", first: nil, code: 0},
		{name: "missing feature", tags: "S-OBJ-14", first: nil, code: 1, stderr: "S-OBJ-01 (P0): no test covers it"},
		{name: "unknown code", tags: "S-OBJ-01, S-ZZZ-01", first: nil, code: 1, stderr: "S-ZZZ-01 is not a code of the catalog"},
		{name: "stale matrix", tags: "S-OBJ-01", first: []string{"-check"}, code: 1, stderr: "is stale"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := repo(t, tt.tags)
			var stdout, stderr bytes.Buffer
			args := append([]string{"-root", root}, tt.first...)
			assert.Equal(t, tt.code, run(args, &stdout, &stderr), stderr.String())
			if tt.stderr != "" {
				assert.Contains(t, stderr.String(), tt.stderr)
				return
			}
			assert.Contains(t, stdout.String(), "wrote test/TRACEABILITY.md")
			stderr.Reset()
			assert.Equal(t, 0, run([]string{"-root", root, "-check"}, &stdout, &stderr), stderr.String())
		})
	}
	var stderr bytes.Buffer
	assert.Equal(t, 2, run([]string{"-unknown"}, &bytes.Buffer{}, &stderr))
}
