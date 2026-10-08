package sysstore

import (
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Defaults of the store.
const (
	// DefaultLockTimeout bounds the wait for the write lock of a document.
	DefaultLockTimeout = 10 * time.Second
	// DefaultGrace is how long a quorum operation waits for the slow
	// drives once it has its quorum: they are not waited for longer (a
	// frozen node must not stall the reads).
	DefaultGrace = 100 * time.Millisecond
)

type options struct {
	root        string
	lockTimeout time.Duration
	grace       time.Duration
}

func defaultOptions() options {
	return options{root: storage.ConfigDir, lockTimeout: DefaultLockTimeout, grace: DefaultGrace}
}

// WithRoot sets the directory of the system volume holding the documents
// (default storage.ConfigDir, "config").
func WithRoot(dir string) Option {
	return func(o *options) {
		if dir = strings.Trim(dir, "/"); dir != "" {
			o.root = dir
		}
	}
}

// WithLockTimeout bounds the wait for the write lock of a document
// (default DefaultLockTimeout).
func WithLockTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.lockTimeout = d
		}
	}
}

// WithGrace sets how long a quorum operation waits for the remaining
// drives once it has its quorum (default DefaultGrace).
func WithGrace(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.grace = d
		}
	}
}
