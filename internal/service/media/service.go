// Package media serves the MP4 videos of the object layer as HLS
// presentations: the playlists and the fMP4 (CMAF) segments are built on
// request from byte ranges of the object, without decoding or re-encoding
// it (docs/proposals/video-streaming.md).
//
// Only the container is parsed: the moov box of a progressive MP4 (H.264
// or H.265 video, AAC audio), read once and kept in memory, gives the
// samples of every track; a segment is the samples between two sync
// samples of the video, about TargetSegment long, repackaged with the
// moof/mdat boxes of a fragment. The segments and the playlists name the
// ETag of the object (the revision): a playlist never mixes two contents
// of a key.
package media

import (
	"context"
	"time"
)

// Service packages the MP4 objects of the object layer as HLS.
type Service interface {
	// Describe reads the presentation of an object: duration, codecs,
	// resolution and segments. An object that is not a supported MP4
	// fails with ErrUnsupported; a revision that is no longer the
	// current one with domain.ErrPreconditionFailed; an SSE-C object with
	// domain.ErrSSECustomerKeyRequired.
	Describe(ctx context.Context, src Source) (*Presentation, error)
	// MasterPlaylist returns the multivariant playlist of src: one
	// variant, the media playlist of the revision Describe reads.
	MasterPlaylist(ctx context.Context, src Source, links Links) ([]byte, error)
	// MediaPlaylist returns the media playlist of src: the init segment
	// and every media segment, with their durations.
	MediaPlaylist(ctx context.Context, src Source, links Links) ([]byte, error)
	// Init returns the initialization segment (ftyp and moov of the
	// fragmented presentation).
	Init(ctx context.Context, src Source) ([]byte, error)
	// Segment returns the media segment n (0-based); a number past the
	// last segment fails with domain.ErrNotFound.
	Segment(ctx context.Context, src Source, n int) ([]byte, error)
}

// Source names an object.
type Source struct {
	Bucket string
	Key    string
	// VersionID selects a version; empty is the current one.
	VersionID string
	// Revision is the ETag the request expects, as the links of a
	// playlist carry it; empty accepts the current content.
	Revision string
}

// Links builds the URIs a playlist refers to, relative to the playlist
// (the routes decide the form: query parameters, a token...).
type Links interface {
	// Media is the URI of the media playlist of revision rev.
	Media(rev string) string
	// Init is the URI of the initialization segment of revision rev.
	Init(rev string) string
	// Segment is the URI of the media segment n of revision rev.
	Segment(rev string, n int) string
}

// Presentation describes the HLS presentation of an object.
type Presentation struct {
	Bucket    string
	Key       string
	VersionID string
	// Revision is the ETag of the content.
	Revision string
	// Size is the size of the object in bytes.
	Size int64
	// Duration is the duration of the video track.
	Duration time.Duration
	// Codecs are the RFC 6381 codec strings of the tracks (video first).
	Codecs []string
	// Width and Height are the dimensions of the video, in pixels.
	Width  int
	Height int
	// FrameRate is the average frame rate of the video (0 when unknown).
	FrameRate float64
	// HasAudio reports an audio track.
	HasAudio bool
	// PeakBandwidth and AverageBandwidth are the bit rates of the
	// segments (EXT-X-STREAM-INF BANDWIDTH and AVERAGE-BANDWIDTH).
	PeakBandwidth    int64
	AverageBandwidth int64
	// Segments are the durations of the media segments.
	Segments []time.Duration
}
