//go:build !linux

package storage

import (
	"errors"
	"os"
)

// directIOSupported reports whether this platform writes the data files of
// a drive with O_DIRECT: Linux only, elsewhere every write goes through
// the page cache as before.
const directIOSupported = false

func setDirectIO(*os.File, bool) error { return errors.ErrUnsupported }

func directIOAlign(string) (int, bool) { return 0, false }
