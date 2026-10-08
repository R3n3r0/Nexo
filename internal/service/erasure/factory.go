package erasure

import (
	"context"
	"sync"
)

// factory caches one codec per layout.
type factory struct {
	deps Deps
	opts options

	mu     sync.Mutex
	codecs map[Layout]*codec
}

var _ Factory = (*factory)(nil)

// NewFactory returns a Factory whose codecs share the options (block size,
// bitrot key, hedge delay).
func NewFactory(deps Deps, opts ...Option) Factory {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &factory{deps: deps, opts: o, codecs: make(map[Layout]*codec)}
}

// Codec implements Factory.
func (f *factory) Codec(ctx context.Context, layout Layout) (Codec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if layout.BlockSize == 0 {
		layout.BlockSize = f.opts.blockSize
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.codecs[layout]; ok {
		return c, nil
	}
	c, err := newCodec(f.deps, f.opts, layout)
	if err != nil {
		return nil, err
	}
	f.codecs[layout] = c
	return c, nil
}
