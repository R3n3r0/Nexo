package cli

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// partAlign is the granularity of the computed part sizes.
const partAlign int64 = 1 << 20

// streamGrowth is the number of parts after which StreamPartSize doubles
// the part size.
const streamGrowth = 1000

// PartSize returns the part size and the number of parts of a multipart
// upload of size bytes (docs/06 TCL-U-06). requested forces the part size
// (0: computed): the computed size is DefaultPartSize, raised to a whole
// number of MiB when size needs more than MaxParts parts. An object of 0
// bytes is one (empty) part. Errors: a size over MaxObjectSize
// (domain.ErrEntityTooLarge), a requested size outside
// [MinPartSize, MaxPartSize] or one that needs more than MaxParts parts
// (ErrUsage).
func PartSize(size, requested int64) (partSize int64, parts int, err error) {
	if size < 0 {
		return 0, 0, fmt.Errorf("%w: negative object size %d", domain.ErrInvalidArgument, size)
	}
	if size > MaxObjectSize {
		return 0, 0, fmt.Errorf("%w: %d bytes exceed the maximum object size of %d bytes", domain.ErrEntityTooLarge, size, MaxObjectSize)
	}
	if requested != 0 {
		if err := checkPartSize(requested); err != nil {
			return 0, 0, err
		}
		n := partsOf(size, requested)
		if n > MaxParts {
			return 0, 0, UsageError("part size %s is too small for %s: %d parts exceed the maximum of %d", HumanSize(requested), HumanSize(size), n, MaxParts)
		}
		return requested, n, nil
	}
	partSize = DefaultPartSize
	if minimum := ceilDiv(size, MaxParts); minimum > partSize {
		partSize = ceilDiv(minimum, partAlign) * partAlign
	}
	return partSize, partsOf(size, partSize), nil
}

// StreamPartSize returns the size of part number n (1-based) of an upload
// of unknown length: base (DefaultPartSize when 0) doubled every 1000
// parts, capped at MaxPartSize, so that a stream may reach MaxObjectSize
// within MaxParts parts.
func StreamPartSize(n int, base int64) int64 {
	if base <= 0 {
		base = DefaultPartSize
	}
	size := base
	for i := (n - 1) / streamGrowth; i > 0 && size < MaxPartSize; i-- {
		size *= 2
	}
	if size > MaxPartSize {
		size = MaxPartSize
	}
	return size
}

// checkPartSize validates a part size chosen by the user.
func checkPartSize(size int64) error {
	if size < MinPartSize || size > MaxPartSize {
		return UsageError("part size %s must be between %s and %s", HumanSize(size), HumanSize(MinPartSize), HumanSize(MaxPartSize))
	}
	return nil
}

// CheckPartSize validates a --part-size flag (0 means automatic).
func CheckPartSize(size int64) error {
	if size == 0 {
		return nil
	}
	return checkPartSize(size)
}

func partsOf(size, partSize int64) int {
	if size == 0 {
		return 1
	}
	return int(ceilDiv(size, partSize))
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
