package scanner

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// UsageFile is the data usage document in the system volume.
const UsageFile = "usage.json"

// Keys of the "scanner" subsystem of the dynamic configuration, read at
// the start of every cycle.
const (
	SubsystemScanner = "scanner"
	KeySpeed         = "speed"
	KeyCycle         = "cycle"
)

// Speeds of the scanner (the "speed" setting): the factor of the sleep
// that follows each page of work, and its upper bound.
const (
	SpeedFastest = "fastest"
	SpeedFast    = "fast"
	SpeedDefault = "default"
	SpeedSlow    = "slow"
	SpeedSlowest = "slowest"
)

// Deps are the dependencies of the scanner.
type Deps struct {
	// Objects is the object layer scanned (required).
	Objects object.Service
	// Healer heals the sampled objects; nil (the single-drive layer)
	// disables the heal sample.
	Healer object.Healer
	// Drives hold usage.json: every online one receives it, the newest
	// copy is read back (the drives of the first erasure set, or the
	// single drive). None keeps the usage in memory only.
	Drives []storage.StorageAPI
	// Leader elects the node that scans; nil scans on this node.
	Leader cluster.Leader
	// Config provides the scanner.speed and scanner.cycle settings; nil
	// keeps the options.
	Config config.Service
	// Load pauses the scan while the API is busy; nil never pauses.
	Load LoadSampler
	// Lifecycle receives every version scanned; nil does nothing.
	Lifecycle Lifecycle
	Clock     clock.Clock
	Logger    *slog.Logger
}

// Option configures the scanner.
type Option func(*options)

type options struct {
	cycle         time.Duration
	startDelay    time.Duration
	speed         string
	healEvery     uint64
	deepEvery     uint64
	staleUploads  time.Duration
	loadThreshold float64
	pauseCheck    time.Duration
	maxPrefixes   int
	pageSize      int
	usageTTL      time.Duration
}

func defaultOptions() options {
	return options{
		cycle:         time.Minute,
		startDelay:    time.Minute,
		speed:         SpeedDefault,
		healEvery:     512,
		deepEvery:     16,
		staleUploads:  24 * time.Hour,
		loadThreshold: 0.8,
		pauseCheck:    time.Second,
		maxPrefixes:   1000,
		pageSize:      1000,
		usageTTL:      5 * time.Second,
	}
}

// WithCycle sets the pause between two cycles (default 1m; the
// scanner.cycle setting overrides it).
func WithCycle(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.cycle = d
		}
	}
}

// WithStartDelay sets the delay before the first cycle after the scanner
// becomes active (default 1m).
func WithStartDelay(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.startDelay = d
		}
	}
}

// WithSpeed sets the pace of the scan (default "default"; the
// scanner.speed setting overrides it). An unknown speed is ignored.
func WithSpeed(speed string) Option {
	return func(o *options) {
		if _, _, err := parseSpeed(speed); err == nil {
			o.speed = strings.ToLower(speed)
		}
	}
}

// WithHealSample heals one object every n scanned (default 512; 0
// disables the sample) and verifies the bitrot hashes of the sample every
// deepEvery cycles (default 16; 0 never).
func WithHealSample(n, deepEvery uint64) Option {
	return func(o *options) { o.healEvery, o.deepEvery = n, deepEvery }
}

// WithStaleUploads sets the age after which a multipart upload is aborted
// (default 24h; 0 disables the cleanup).
func WithStaleUploads(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.staleUploads = d
		}
	}
}

// WithLoadThreshold sets the API load (0..1) at or above which the scan
// pauses, and how often the load is checked again while paused (default
// 0.8, 1s).
func WithLoadThreshold(threshold float64, check time.Duration) Option {
	return func(o *options) {
		if threshold > 0 {
			o.loadThreshold = threshold
		}
		if check > 0 {
			o.pauseCheck = check
		}
	}
}

// WithMaxPrefixes bounds the prefixes of a bucket in the usage (default
// 1000).
func WithMaxPrefixes(n int) Option {
	return func(o *options) {
		if n >= 0 {
			o.maxPrefixes = n
		}
	}
}

// WithPageSize sets the number of keys listed per page (default 1000).
func WithPageSize(n int) Option {
	return func(o *options) {
		if n > 0 && n <= 1000 {
			o.pageSize = n
		}
	}
}

// parseSpeed returns the sleep factor and bound of a speed.
func parseSpeed(speed string) (factor float64, maxSleep time.Duration, err error) {
	switch strings.ToLower(strings.TrimSpace(speed)) {
	case SpeedFastest:
		return 0, 0, nil
	case SpeedFast:
		return 1, 100 * time.Millisecond, nil
	case "", SpeedDefault:
		return 2, time.Second, nil
	case SpeedSlow:
		return 10, 15 * time.Second, nil
	case SpeedSlowest:
		return 100, 15 * time.Second, nil
	}
	return 0, 0, fmt.Errorf("%w: unknown scanner speed %q", domain.ErrInvalidArgument, speed)
}
