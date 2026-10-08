package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRun checks the wiring of the process: the exit codes of docs/06
// TCL-U-04 reach the caller and the outputs go to the given streams.
func TestRun(t *testing.T) {
	t.Setenv("NEXOCTL_CONFIG_DIR", t.TempDir())
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{name: "version", args: []string{"--version"}, code: 0, stdout: "nexoctl"},
		{name: "help", args: []string{"--help"}, code: 0, stdout: "Usage:"},
		{name: "unknown command", args: []string{"nosuchcommand"}, code: 2, stderr: "nosuchcommand"},
		{name: "unknown alias", args: []string{"ls", "nosuchalias/bucket"}, code: 3, stderr: "nosuchalias"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			assert.Equal(t, tt.code, run(tt.args, &stdout, &stderr), "stdout: %s\nstderr: %s", stdout.String(), stderr.String())
			assert.Contains(t, stdout.String(), tt.stdout)
			assert.Contains(t, stderr.String(), tt.stderr)
		})
	}
}
