//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// replaceProcess replaces the process with the binary at path, with the
// same arguments and environment. If the new binary cannot be executed the
// previous one (path.old, kept by the update) is put back and executed.
func replaceProcess(path string) error {
	err := syscall.Exec(path, os.Args, os.Environ()) //nolint:gosec // G204: the binary installed by an update of an administrator
	old := path + ".old"
	if _, serr := os.Stat(old); serr != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	failed := path + ".failed"
	if rerr := os.Rename(path, failed); rerr != nil {
		return fmt.Errorf("exec %s: %w (restore %s: %v)", path, err, old, rerr)
	}
	if rerr := os.Rename(old, path); rerr != nil {
		return fmt.Errorf("exec %s: %w (restore %s: %v)", path, err, old, rerr)
	}
	fmt.Fprintf(os.Stderr, "error: exec %s: %v: the previous binary is restored (the new one is kept as %s)\n", path, err, failed)
	return fmt.Errorf("exec the restored binary %s: %w", path, syscall.Exec(path, os.Args, os.Environ())) //nolint:gosec // G204: the previous binary
}
