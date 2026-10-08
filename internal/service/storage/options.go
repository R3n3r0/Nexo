package storage

// Option configures the local storage implementation.
type Option func(*options)

type options struct {
	// fsync controls whether data and metadata writes are synced to disk
	// before being committed (default true).
	fsync bool
	// cleanTmp empties the system tmp directory at startup (default true).
	cleanTmp bool
	// dirPerm and filePerm are the permissions of created entries.
	dirPerm  uint32
	filePerm uint32
}

func defaultOptions() options {
	return options{fsync: true, cleanTmp: true, dirPerm: 0o750, filePerm: 0o640}
}

// WithoutFsync skips fsync calls; data durability is then left to the
// operating system (tests, benchmarks).
func WithoutFsync() Option {
	return func(o *options) { o.fsync = false }
}

// WithoutTmpCleanup keeps the content of the system tmp directory at startup.
func WithoutTmpCleanup() Option {
	return func(o *options) { o.cleanTmp = false }
}
