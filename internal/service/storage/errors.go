package storage

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ErrFileCorrupt reports a file whose content does not match what its
// metadata promises (truncated part, bad xl.meta checksum). It wraps
// domain.ErrFaultyDisk: the drive copy is unusable and must be healed.
var ErrFileCorrupt = fmt.Errorf("%w: file corrupt", domain.ErrFaultyDisk)

// errSymlink reports a symbolic link met while resolving a path inside the
// drive: links are never followed, so that no operation escapes the drive.
var errSymlink = fmt.Errorf("%w: symbolic link in drive path", domain.ErrAccessDenied)
