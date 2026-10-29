package app

import (
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/playback"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	jssdk "github.com/r3n3r0/nexo/sdk/js"
)

// newPlayback builds the playback token service on the system documents
// (in SNSD, a system store on the single drive), its tokens signed with
// a key derived from the media protection key.
func newPlayback(d composeDeps, docs sysstore.Store, buckets bucket.Service, key []byte, registry metrics.Registry, logger *slog.Logger) (playback.Service, error) {
	logger = logger.With("component", "playback")
	if docs == nil {
		if len(d.f.local) == 0 {
			return nil, fmt.Errorf("%w: playback: no local drive", domain.ErrInvalidArgument)
		}
		var err error
		if docs, err = sysstore.New(sysstore.Deps{Drives: d.f.local[:1], Locks: d.f.locks, Clock: d.clock, Logger: logger}); err != nil {
			return nil, err
		}
	}
	return playback.New(playback.Deps{Protections: buckets, Store: docs, Metrics: registry, Clock: d.clock, Logger: logger},
		playback.WithKey(key))
}

// jsSDK returns the browser modules of the JavaScript SDK the S3 listener
// serves under /nexo/media/sdk/.
func jsSDK() fs.FS {
	sub, err := fs.Sub(jssdk.Files, "src")
	if err != nil {
		return nil
	}
	return sub
}
