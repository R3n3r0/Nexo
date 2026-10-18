package media

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ErrUnsupported reports an object that cannot be streamed: not an MP4,
// a fragmented MP4, a codec HLS players do not play, or a file over the
// limits of the service.
var ErrUnsupported = fmt.Errorf("%w: media", domain.ErrInvalidArgument)
