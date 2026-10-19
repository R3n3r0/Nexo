package domain

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// MediaSidecarPrefix is the reserved prefix of a bucket under which the
// media worker writes what it derives from a video (renditions, poster,
// thumbnails, status.json): ".nexo-media/<key>/<versionId>/". Listings of
// the console and of nexoctl hide it by default; the S3 API lists it like
// any prefix, so backups and mirrors keep it.
const MediaSidecarPrefix = ".nexo-media/"

// MediaNullVersion names the sidecar directory of the null version (an
// unversioned bucket).
const MediaNullVersion = "null"

// MediaStatusFile is the job document of the media worker in the sidecar
// directory of a video.
const MediaStatusFile = "status.json"

// Media sidecar files and directories.
const (
	MediaRenditionsDir  = "renditions/"
	MediaPosterFile     = "poster.jpg"
	MediaSpriteFile     = "thumbnails/sprite.jpg"
	MediaThumbnailsFile = "thumbnails/thumbnails.vtt"
)

// MediaSidecarDir returns the sidecar directory of a version of a video,
// with its trailing slash.
func MediaSidecarDir(key, versionID string) string {
	if versionID == "" {
		versionID = MediaNullVersion
	}
	return MediaSidecarPrefix + key + "/" + versionID + "/"
}

// IsMediaSidecar reports whether key is under MediaSidecarPrefix.
func IsMediaSidecar(key string) bool { return strings.HasPrefix(key, MediaSidecarPrefix) }

// MediaConfig is the video configuration of a bucket: its rules select the
// videos that the media worker processes and say what it derives from
// them. Streaming the source (HLS packaged on the fly) needs no rule.
type MediaConfig struct {
	Rules []MediaRule `json:"rules"`
}

// MediaRule selects videos by key prefix and suffix.
type MediaRule struct {
	// ID names the rule (1-64 letters, digits, '.', '_' or '-').
	ID string `json:"id"`
	// Prefix of the keys (empty: the whole bucket).
	Prefix string `json:"prefix,omitempty"`
	// Suffixes of the keys, matched without case (default
	// DefaultMediaSuffixes).
	Suffixes []string `json:"suffixes,omitempty"`
	// Renditions are the lower qualities to produce (MediaRenditionPreset
	// names); those taller than the source are skipped.
	Renditions []string `json:"renditions,omitempty"`
	// Poster asks for a JPEG poster frame.
	Poster bool `json:"poster,omitempty"`
	// Thumbnails asks for a timeline thumbnail sprite with its WebVTT map.
	Thumbnails *MediaThumbnails `json:"thumbnails,omitempty"`
}

// MediaThumbnails configures the timeline thumbnails.
type MediaThumbnails struct {
	// Interval between two thumbnails, a Go duration (1s to 10m; default
	// 10s).
	Interval string `json:"interval,omitempty"`
}

// IntervalDuration returns the interval of the thumbnails (10s when
// unset).
func (t MediaThumbnails) IntervalDuration() time.Duration {
	d, err := time.ParseDuration(t.Interval)
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

// DefaultMediaSuffixes are the suffixes of a rule that names none.
func DefaultMediaSuffixes() []string { return []string{".mp4", ".mov", ".m4v"} }

// MediaRendition is a quality the media worker can produce: H.264/AAC at
// most Height pixels tall (the width follows the aspect ratio).
type MediaRendition struct {
	Name         string
	Height       int
	VideoBitrate int // bits per second
	AudioBitrate int // bits per second
}

// MediaRenditionNames are the rendition presets, tallest first.
func MediaRenditionNames() []string {
	return []string{"2160p", "1440p", "1080p", "720p", "480p", "360p", "240p"}
}

// MediaRenditionPreset returns the preset of a rendition name.
func MediaRenditionPreset(name string) (MediaRendition, bool) {
	r := MediaRendition{Name: name, AudioBitrate: 128_000}
	switch name {
	case "2160p":
		r.Height, r.VideoBitrate = 2160, 16_000_000
	case "1440p":
		r.Height, r.VideoBitrate = 1440, 8_000_000
	case "1080p":
		r.Height, r.VideoBitrate = 1080, 5_000_000
	case "720p":
		r.Height, r.VideoBitrate = 720, 2_800_000
	case "480p":
		r.Height, r.VideoBitrate, r.AudioBitrate = 480, 1_400_000, 96_000
	case "360p":
		r.Height, r.VideoBitrate, r.AudioBitrate = 360, 800_000, 96_000
	case "240p":
		r.Height, r.VideoBitrate, r.AudioBitrate = 240, 400_000, 64_000
	default:
		return MediaRendition{}, false
	}
	return r, true
}

// Clone returns a deep copy of the configuration.
func (c MediaConfig) Clone() MediaConfig {
	out := MediaConfig{Rules: make([]MediaRule, len(c.Rules))}
	for i, r := range c.Rules {
		out.Rules[i] = r.Clone()
	}
	return out
}

// Clone returns a deep copy of the rule.
func (r MediaRule) Clone() MediaRule {
	r.Suffixes = slices.Clone(r.Suffixes)
	r.Renditions = slices.Clone(r.Renditions)
	if r.Thumbnails != nil {
		t := *r.Thumbnails
		r.Thumbnails = &t
	}
	return r
}

// maxMediaRules bounds the rules of a bucket.
const maxMediaRules = 100

func mediaRuleID() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`) }

// Validate checks the configuration and normalizes it in place: suffixes
// lowercased with their default, renditions deduplicated tallest first.
func (c *MediaConfig) Validate() error {
	if len(c.Rules) > maxMediaRules {
		return fmt.Errorf("%w: at most %d media rules", ErrInvalidArgument, maxMediaRules)
	}
	ids := map[string]bool{}
	valid := mediaRuleID()
	for i := range c.Rules {
		r := &c.Rules[i]
		if !valid.MatchString(r.ID) {
			return fmt.Errorf("%w: media rule %d: the id must be 1-64 letters, digits, '.', '_' or '-'", ErrInvalidArgument, i+1)
		}
		if ids[r.ID] {
			return fmt.Errorf("%w: media rule %q: duplicate id", ErrInvalidArgument, r.ID)
		}
		ids[r.ID] = true
		if len(r.Prefix) > 1024 {
			return fmt.Errorf("%w: media rule %q: the prefix is longer than 1024 bytes", ErrInvalidArgument, r.ID)
		}
		if IsMediaSidecar(r.Prefix) {
			return fmt.Errorf("%w: media rule %q: the prefix %s is reserved", ErrInvalidArgument, r.ID, MediaSidecarPrefix)
		}
		if len(r.Suffixes) == 0 {
			r.Suffixes = DefaultMediaSuffixes()
		}
		for j, s := range r.Suffixes {
			s = strings.ToLower(strings.TrimSpace(s))
			if s == "" || len(s) > 32 {
				return fmt.Errorf("%w: media rule %q: invalid suffix %q", ErrInvalidArgument, r.ID, r.Suffixes[j])
			}
			r.Suffixes[j] = s
		}
		r.Suffixes = slices.Compact(slices.Sorted(slices.Values(r.Suffixes)))
		var renditions []string
		for _, name := range MediaRenditionNames() {
			if slices.Contains(r.Renditions, name) {
				renditions = append(renditions, name)
			}
		}
		for _, name := range r.Renditions {
			if _, ok := MediaRenditionPreset(name); !ok {
				return fmt.Errorf("%w: media rule %q: unknown rendition %q (one of %s)", ErrInvalidArgument, r.ID, name,
					strings.Join(MediaRenditionNames(), ", "))
			}
		}
		r.Renditions = renditions
		if r.Thumbnails != nil {
			if r.Thumbnails.Interval == "" {
				r.Thumbnails.Interval = "10s"
			}
			d, err := time.ParseDuration(r.Thumbnails.Interval)
			if err != nil || d < time.Second || d > 10*time.Minute {
				return fmt.Errorf("%w: media rule %q: the thumbnail interval must be a duration from 1s to 10m", ErrInvalidArgument, r.ID)
			}
			r.Thumbnails.Interval = d.String()
		}
		if len(r.Renditions) == 0 && !r.Poster && r.Thumbnails == nil {
			return fmt.Errorf("%w: media rule %q: nothing to produce (renditions, poster or thumbnails)", ErrInvalidArgument, r.ID)
		}
	}
	return nil
}

// Match returns the first rule selecting key; the sidecars never match.
func (c MediaConfig) Match(key string) (MediaRule, bool) {
	if IsMediaSidecar(key) {
		return MediaRule{}, false
	}
	lower := strings.ToLower(key)
	for _, r := range c.Rules {
		if !strings.HasPrefix(key, r.Prefix) {
			continue
		}
		suffixes := r.Suffixes
		if len(suffixes) == 0 {
			suffixes = DefaultMediaSuffixes()
		}
		for _, s := range suffixes {
			if strings.HasSuffix(lower, s) {
				return r, true
			}
		}
	}
	return MediaRule{}, false
}

// MediaJobAction is what a media job asks the worker to do.
type MediaJobAction string

// Media job actions.
const (
	// MediaJobProcess derives the outputs of the rule from a new version.
	MediaJobProcess MediaJobAction = "process"
	// MediaJobDelete removes the sidecars of a version that is gone.
	MediaJobDelete MediaJobAction = "delete"
	// MediaJobTest checks the delivery (the test event of the target).
	MediaJobTest MediaJobAction = "test"
)

// MediaJob is a job sent to the media worker.
type MediaJob struct {
	Action    MediaJobAction `json:"action"`
	Bucket    string         `json:"bucket"`
	Key       string         `json:"key"`
	VersionID string         `json:"versionId,omitempty"`
	ETag      string         `json:"etag,omitempty"`
	Size      int64          `json:"size,omitempty"`
	// Sidecar is the sidecar directory of the version (MediaSidecarDir).
	// A job without VersionID (a reprocessing of the current version)
	// has none: the worker reads the current version and names it.
	Sidecar string `json:"sidecar"`
	// Rule is the rule that selected the video (MediaJobProcess).
	Rule *MediaRule `json:"rule,omitempty"`
	Time time.Time  `json:"time"`
}

// MediaJobState is the state of the job of a video.
type MediaJobState string

// Media job states.
const (
	MediaStateProcessing MediaJobState = "processing"
	MediaStateDone       MediaJobState = "done"
	MediaStateFailed     MediaJobState = "failed"
)

// MediaStatus is the status.json document the worker keeps in the sidecar
// directory of a video. The server uses the outputs only while ETag is the
// ETag of the version they were derived from.
type MediaStatus struct {
	State  MediaJobState `json:"state"`
	Error  string        `json:"error,omitempty"`
	Rule   string        `json:"rule,omitempty"`
	ETag   string        `json:"etag"`
	Step   string        `json:"step,omitempty"`
	Worker string        `json:"worker,omitempty"`
	// Duration of the source in seconds.
	Duration   float64                `json:"duration,omitempty"`
	Width      int                    `json:"width,omitempty"`
	Height     int                    `json:"height,omitempty"`
	Renditions []MediaRenditionOutput `json:"renditions,omitempty"`
	Poster     bool                   `json:"poster,omitempty"`
	Thumbnails *MediaThumbnailsOutput `json:"thumbnails,omitempty"`
	Started    time.Time              `json:"started"`
	Updated    time.Time              `json:"updated"`
}

// MediaRenditionOutput is a rendition produced by the worker
// (MediaRenditionsDir + Name + ".mp4" in the sidecar directory).
type MediaRenditionOutput struct {
	Name      string `json:"name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Bandwidth int    `json:"bandwidth"`
}

// MediaThumbnailsOutput describes the thumbnail sprite: a grid of Columns
// tiles of Width x Height, one every Interval seconds, mapped by
// MediaThumbnailsFile.
type MediaThumbnailsOutput struct {
	Interval float64 `json:"interval"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Columns  int     `json:"columns"`
	Count    int     `json:"count"`
}
