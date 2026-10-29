package media

import "time"

// Defaults of the options.
const (
	// DefaultTargetSegment is the duration a segment aims at: it starts at
	// the first sync sample at or after a multiple of it.
	DefaultTargetSegment = 6 * time.Second
	// DefaultMaxIndex bounds the moov box read into memory (about four
	// hours of 30 fps video with stereo audio).
	DefaultMaxIndex = 64 << 20
	// DefaultMaxSegment bounds the bytes read for one segment.
	DefaultMaxSegment = 256 << 20
	// DefaultCacheSize bounds the memory of the parsed presentations.
	DefaultCacheSize = 256 << 20
	// maxTopLevelBoxes bounds the boxes walked to find the moov box.
	maxTopLevelBoxes = 1024
	// maxSamples bounds the samples of a track (a day of 60 fps video).
	maxSamples = 6_000_000
	// maxSegments bounds the segments of a presentation (a week of 6 s
	// segments).
	maxSegments = 100_000
)

// Option configures the service.
type Option func(*options)

type options struct {
	targetSegment time.Duration
	maxIndex      int64
	maxSegment    int64
	cacheSize     int64
	// protectionKey derives the keys of the protected videos
	// (WithProtectionKey); empty protects nothing.
	protectionKey []byte
}

func defaultOptions() options {
	return options{
		targetSegment: DefaultTargetSegment,
		maxIndex:      DefaultMaxIndex,
		maxSegment:    DefaultMaxSegment,
		cacheSize:     DefaultCacheSize,
	}
}

// WithTargetSegment sets the duration the segments aim at (at least one
// second; DefaultTargetSegment otherwise).
func WithTargetSegment(d time.Duration) Option {
	return func(o *options) {
		if d >= time.Second {
			o.targetSegment = d
		}
	}
}

// WithMaxIndex bounds the size of the moov box (positive values only).
func WithMaxIndex(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.maxIndex = n
		}
	}
}

// WithMaxSegment bounds the bytes read for one segment (positive values
// only).
func WithMaxSegment(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.maxSegment = n
		}
	}
}

// WithCacheSize bounds the memory of the parsed presentations (positive
// values only).
func WithCacheSize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.cacheSize = n
		}
	}
}

// WithProtectionKey sets the media protection key of the deployment
// (DeriveProtectionKey): the segments of the protected videos are
// encrypted with keys derived from it. Without it no video is protected.
func WithProtectionKey(key []byte) Option {
	return func(o *options) {
		if len(key) >= 16 {
			o.protectionKey = append([]byte(nil), key...)
		}
	}
}
