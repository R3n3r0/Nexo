package main

import (
	"os"
	"path/filepath"
)

// executable returns the path of the binary of the process, its symbolic
// links resolved: an update replaces that file.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
