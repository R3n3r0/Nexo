package commands_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli/commands"
)

// covers: TCL-U-05
func TestHelpGolden(t *testing.T) {
	h := newHarness(t)
	root := commands.NewRootCmd(h.deps)
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpCmd()
	var paths [][]string
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		if c.Hidden || c.Name() == "help" {
			return
		}
		paths = append(paths, path)
		for _, sub := range c.Commands() {
			walk(sub, append(append([]string{}, path...), sub.Name()))
		}
	}
	walk(root, nil)
	require.Greater(t, len(paths), 40, "every command has a golden help")
	for _, path := range paths {
		name := "help/nexoctl"
		if len(path) > 0 {
			name += "_" + strings.Join(path, "_")
		}
		t.Run(name, func(t *testing.T) {
			r := h.run(append(append([]string{}, path...), "--help")...)
			require.Equal(t, 0, r.code, r.stderr)
			assert.Empty(t, r.stderr)
			golden(t, name, r.stdout)
		})
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	r := h.mustRun("version")
	assert.True(t, strings.HasPrefix(r.stdout, "nexoctl dev"), r.stdout)
	r = h.mustRun("version", "--json")
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	assert.Equal(t, "dev", recs[0]["version"])
	assert.Contains(t, recs[0], "goVersion")
	r = h.mustRun("--version")
	assert.Equal(t, "nexoctl dev\n", r.stdout)
}
