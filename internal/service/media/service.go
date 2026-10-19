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
	"io"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
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
	// Status returns the job status of the media worker for the version
	// of src: nil when the worker never processed it.
	Status(ctx context.Context, src Source) (*OutputStatus, error)
	// Sidecar returns a file the media worker derived from the version of
	// src (domain.MediaPosterFile, MediaSpriteFile or
	// MediaThumbnailsFile): domain.ErrNotFound when it did not, or not
	// from the current content.
	Sidecar(ctx context.Context, src Source, file string) ([]byte, error)
	// Image returns the variant of an image the media worker derived from
	// the version of src that req selects (SelectImage):
	// domain.ErrNotFound when the worker derived none from the current
	// content, or none in the format req names; domain.ErrInvalidArgument
	// for an unknown format or a malformed width.
	Image(ctx context.Context, src Source, req ImageRequest) (*Image, error)
}

// ImageFormatAuto is the format of an ImageRequest that negotiates the
// format with the Accept header of the client.
const ImageFormatAuto = "auto"

// ImageRequest selects a variant of an image (SelectImage).
type ImageRequest struct {
	// Width is the width the client displays, in pixels: the smallest
	// variant at least that wide, else the largest. 0 asks for the image
	// at its own size.
	Width int
	// Format is a domain.MediaImageFormats name ("jpg" is jpeg), or
	// ImageFormatAuto (or empty): the most compact format the client
	// accepts (ImageAccepts).
	Format string
	// Accept is the Accept header of the client (format auto).
	Accept string
}

// Image is a variant of an image served to a client.
type Image struct {
	domain.MediaImageOutput
	// VersionID is the version of the image the variant derives from.
	VersionID string
	// ContentType is the media type of the format.
	ContentType string
	// ETag identifies the variant: the ETag of the source, the format and
	// the width (quoted).
	ETag string
	// Modified is when the worker finished the variants.
	Modified time.Time
	// Content reads the variant (Size bytes) from any offset; the caller
	// closes it.
	Content io.ReadSeekCloser
}

// OutputStatus is the status of the outputs of the media worker for a
// version of a video.
type OutputStatus struct {
	domain.MediaStatus
	// VersionID is the version the status is about.
	VersionID string
	// Current reports outputs derived from the current content of the
	// version (done, for its ETag): only they are served.
	Current bool
}

// Jobs derives the jobs of the media worker from the events of the object
// layer: it decorates the event service, and for every creation of an
// object a media rule of its bucket selects (domain.MediaConfig.Match) it
// publishes a job (event.MediaJob) deriving the outputs of the version;
// for every permanent removal of such a version, a job removing its
// sidecars. The jobs reach the worker through the media_worker targets.
type Jobs interface {
	event.Service
	// Reprocess publishes a job deriving the outputs of a version of a
	// video again ("" is the current version): domain.ErrInvalidArgument
	// when no media rule of the bucket selects the key.
	Reprocess(ctx context.Context, bucket, key, versionID string) error
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
	// Rendition names a rendition the media worker produced (a
	// domain.MediaRenditionPreset name): the presentation is that of the
	// rendition, whose ETag Revision then is. Empty is the source.
	Rendition string
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
	// Rendition is the URI of the media playlist of a rendition of the
	// media worker, rev being the ETag of the rendition.
	Rendition(name, rev string) string
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
