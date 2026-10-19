package app

import (
	"context"
	"log/slog"
	"strconv"
	"sync"

	"github.com/r3n3r0/nexo/internal/lifecycle"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/settings"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// metaLogTuning applies the storage subsystem of the configuration (the
// segment size and the memory budget of the metadata log) to the local
// drives: at start, then at every change. Drives without the log ignore it.
func metaLogTuning(dyn configsvc.Service, drives []storage.MetadataLogTuner, logger *slog.Logger) lifecycle.Runner {
	var mu sync.Mutex
	var cancel context.CancelFunc
	var done chan struct{}
	apply := func(ctx context.Context) {
		kv, err := dyn.Get(ctx, settings.SubsystemStorage)
		if err != nil {
			kv = nil // unset: the defaults
		}
		seg := int64(storage.DefaultMetaLogSegmentSize)
		if n, err := strconv.ParseInt(kv[settings.KeyMetadataLogSegmentMiB], 10, 64); err == nil && n > 0 {
			seg = n << 20
		}
		mem := storage.DefaultMetaLogMemoryPercent
		if n, err := strconv.Atoi(kv[settings.KeyMetadataLogMemoryPercent]); err == nil && n > 0 {
			mem = n
		}
		for _, d := range drives {
			d.SetMetadataLogSegmentSize(seg)
			d.SetMetadataLogMemoryPercent(mem)
		}
		logger.Debug("storage: metadata log settings applied", "segment_bytes", seg, "memory_percent", mem)
	}
	return lifecycle.Funcs{
		OnStart: func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if len(drives) == 0 || cancel != nil {
				return nil
			}
			runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
			changes := dyn.Watch(runCtx)
			apply(ctx)
			cancel, done = stop, make(chan struct{})
			go func(done chan<- struct{}) {
				defer close(done)
				for ch := range changes {
					if ch.Subsystem == "" || ch.Subsystem == settings.SubsystemStorage {
						apply(runCtx)
					}
				}
			}(done)
			return nil
		},
		OnStop: func(context.Context) error {
			mu.Lock()
			stop, wait := cancel, done
			cancel, done = nil, nil
			mu.Unlock()
			if stop != nil {
				stop()
				<-wait
			}
			return nil
		},
	}
}
