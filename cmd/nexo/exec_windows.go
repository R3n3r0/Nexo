//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// replaceProcess starts the binary at path with the same arguments,
// environment and standard streams, then lets this process exit: Windows
// cannot replace the image of a running process.
func replaceProcess(path string) error {
	cmd := exec.Command(path, os.Args[1:]...) //nolint:gosec // G204: the binary installed by an update of an administrator
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, os.Environ()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", path, err)
	}
	return cmd.Process.Release()
}
