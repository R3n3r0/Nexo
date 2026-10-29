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
// thumbnails, status.json) or an image (its variants): ".nexo-media/<key>/<versionId>/". Listings of
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
	MediaImagesDir      = "images/"
)

// MediaImageFile returns the sidecar file of an image variant: width
// pixels wide ("images/640.webp"), or the image at its own size when width
// is 0 ("images/full.webp").
func MediaImageFile(width int, format string) string {
	name := "full"
	if width > 0 {
		name = fmt.Sprint(width)
	}
	return MediaImagesDir + name + "." + MediaImageExtension(format)
}

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

// MediaSidecarSource returns the key of the object a sidecar file derives
// from (the inverse of MediaSidecarDir followed by a file of the
// directory: status.json, poster.jpg, images/<name>, renditions/<name>,
// thumbnails/<name>) and the file; ok is false for a key that is not
// such a file.
func MediaSidecarSource(key string) (source, file string, ok bool) {
	rest, found := strings.CutPrefix(key, MediaSidecarPrefix)
	if !found {
		return "", "", false
	}
	segs := strings.Split(rest, "/")
	// <key...>/<version>/<file> or <key...>/<version>/<dir>/<file>.
	n := 1
	if len(segs) >= 4 {
		switch segs[len(segs)-2] + "/" {
		case MediaImagesDir, MediaRenditionsDir, "thumbnails/", MediaSubtitlesDir:
			n = 2
		}
	}
	if len(segs) < n+2 || segs[len(segs)-1] == "" || segs[len(segs)-n-1] == "" {
		return "", "", false
	}
	source = strings.Join(segs[:len(segs)-n-1], "/")
	if source == "" {
		return "", "", false
	}
	return source, strings.Join(segs[len(segs)-n:], "/"), true
}

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
	// Subtitles asks for automatic subtitles: the speech transcribed and
	// translated into the languages it names (needs a worker with a
	// transcription backend).
	Subtitles *MediaSubtitles `json:"subtitles,omitempty"`
	// Images makes an image rule: it selects images (default suffixes
	// DefaultImageSuffixes) and derives their variants, in every format
	// and width it names. An image rule has no video output.
	Images *MediaImages `json:"images,omitempty"`
}

// MediaImages configures the variants of an image rule.
type MediaImages struct {
	// Formats of the variants (MediaImageFormats names; default all of
	// them).
	Formats []string `json:"formats,omitempty"`
	// Widths of the variants in pixels, 16 to 8192 (default
	// DefaultImageWidths). Widths not smaller than the image are skipped;
	// every format also gets the image at its own size.
	Widths []int `json:"widths,omitempty"`
}

// MediaImageFormats are the formats of the image variants, the most
// compact first: the order in which a client accepting several is served.
func MediaImageFormats() []string { return []string{"avif", "webp", "jpeg", "png"} }

// MediaImageExtension returns the file extension of an image format.
func MediaImageExtension(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return format
}

// MediaImageContentType returns the media type of an image format.
func MediaImageContentType(format string) string { return "image/" + format }

// DefaultImageWidths are the widths of an image rule that names none:
// thumbnails to large screens.
func DefaultImageWidths() []int { return []int{160, 320, 640, 1280, 1920, 2560} }

// DefaultImageSuffixes are the suffixes of an image rule that names none:
// the images the media worker decodes.
func DefaultImageSuffixes() []string {
	return []string{".avif", ".bmp", ".gif", ".heic", ".heif", ".jpeg", ".jpg", ".png", ".tif", ".tiff", ".webp"}
}

// ImagesMediaRuleID is the ID of the rule ImagesMediaRule returns.
const ImagesMediaRuleID = "images"

// ImagesMediaRule is the rule of a bucket created for images (the images
// option of bucket creation: console, Admin API, nexoctl mb
// --with-images): every image of the bucket gets every format at every
// width up to its own.
func ImagesMediaRule() MediaRule {
	return MediaRule{ID: ImagesMediaRuleID, Images: &MediaImages{Formats: MediaImageFormats(), Widths: DefaultImageWidths()}}
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

// HLSMediaRuleID is the ID of the rule HLSMediaRule returns.
const HLSMediaRuleID = "hls"

// HLSMediaRule is the rule of a bucket created for video (the HLS option of
// bucket creation: console, Admin API, nexoctl mb --with-hls): every video
// of the bucket gets every rendition up to its own height, a poster and
// timeline thumbnails.
func HLSMediaRule() MediaRule {
	return MediaRule{ID: HLSMediaRuleID, Renditions: MediaRenditionNames(), Poster: true,
		Thumbnails: &MediaThumbnails{Interval: "10s"}}
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
	if r.Images != nil {
		img := MediaImages{Formats: slices.Clone(r.Images.Formats), Widths: slices.Clone(r.Images.Widths)}
		r.Images = &img
	}
	if r.Subtitles != nil {
		s := MediaSubtitles{Source: r.Subtitles.Source, Languages: slices.Clone(r.Subtitles.Languages)}
		r.Subtitles = &s
	}
	return r
}

// defaultSuffixes returns the suffixes of the rule when it names none.
func (r MediaRule) defaultSuffixes() []string {
	if r.Images != nil {
		return DefaultImageSuffixes()
	}
	return DefaultMediaSuffixes()
}

// maxImageWidths bounds the widths of an image rule.
const maxImageWidths = 16

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
			r.Suffixes = r.defaultSuffixes()
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
		if r.Subtitles != nil {
			if err := r.Subtitles.validate(); err != nil {
				return fmt.Errorf("%w: media rule %q: %s", ErrInvalidArgument, r.ID, strings.TrimPrefix(err.Error(), ErrInvalidArgument.Error()+": "))
			}
		}
		if r.Images != nil {
			if len(r.Renditions) > 0 || r.Poster || r.Thumbnails != nil || r.Subtitles != nil {
				return fmt.Errorf("%w: media rule %q: an image rule has no renditions, poster, thumbnails or subtitles", ErrInvalidArgument, r.ID)
			}
			if err := r.Images.validate(); err != nil {
				return fmt.Errorf("%w: media rule %q: %s", ErrInvalidArgument, r.ID, err)
			}
			continue
		}
		if len(r.Renditions) == 0 && !r.Poster && r.Thumbnails == nil && r.Subtitles == nil {
			return fmt.Errorf("%w: media rule %q: nothing to produce (renditions, poster, thumbnails or subtitles)", ErrInvalidArgument, r.ID)
		}
	}
	return nil
}

// validate normalizes the variants: formats in MediaImageFormats order
// (all when none), widths deduplicated in ascending order (the defaults
// when none).
func (m *MediaImages) validate() error {
	for _, f := range m.Formats {
		if !slices.Contains(MediaImageFormats(), f) {
			return fmt.Errorf("unknown image format %q (one of %s)", f, strings.Join(MediaImageFormats(), ", "))
		}
	}
	var formats []string
	for _, f := range MediaImageFormats() {
		if len(m.Formats) == 0 || slices.Contains(m.Formats, f) {
			formats = append(formats, f)
		}
	}
	m.Formats = formats
	if len(m.Widths) == 0 {
		m.Widths = DefaultImageWidths()
	}
	for _, w := range m.Widths {
		if w < 16 || w > 8192 {
			return fmt.Errorf("the image widths must be from 16 to 8192 pixels, not %d", w)
		}
	}
	m.Widths = slices.Compact(slices.Sorted(slices.Values(m.Widths)))
	if len(m.Widths) > maxImageWidths {
		return fmt.Errorf("at most %d image widths", maxImageWidths)
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
			suffixes = r.defaultSuffixes()
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
	// Rule is the rule that selected the video or the image
	// (MediaJobProcess).
	Rule *MediaRule `json:"rule,omitempty"`
	// Forensic asks for the B variant of every rendition (the forensic
	// watermark of the protection of the bucket, MediaProtection.Forensic).
	Forensic bool      `json:"forensic,omitempty"`
	Time     time.Time `json:"time"`
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
	// Images are the variants of an image (Width and Height are the size
	// of the image, after its orientation).
	Images []MediaImageOutput `json:"images,omitempty"`
	// Alpha reports an image with transparency: its JPEG variants are
	// flattened, so clients that cannot take AVIF or WebP get PNG.
	Alpha bool `json:"alpha,omitempty"`
	// Subtitles are the languages of the automatic subtitle tracks the
	// run made (MediaSubtitlesManifest describes every track).
	Subtitles []string `json:"subtitles,omitempty"`
	// Language is the spoken language the transcription detected.
	Language string `json:"language,omitempty"`
	// SubtitlesError is why the automatic subtitles failed (the other
	// outputs do not depend on them: the job is done nonetheless).
	SubtitlesError string    `json:"subtitlesError,omitempty"`
	Started        time.Time `json:"started"`
	Updated        time.Time `json:"updated"`
}

// MediaRenditionOutput is a rendition produced by the worker
// (MediaRenditionsDir + Name + ".mp4" in the sidecar directory).
type MediaRenditionOutput struct {
	Name      string `json:"name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Bandwidth int    `json:"bandwidth"`
	// Forensic reports the B variant of the rendition
	// (MediaRenditionFile(Name, true)): the same pictures with the
	// opposite invisible mark, its samples and segments aligned with the
	// A variant.
	Forensic bool `json:"forensic,omitempty"`
}

// MediaRenditionFile is the file of a rendition in the renditions
// directory: <name>.mp4, the B variant of the forensic watermark
// <name>.b.mp4.
func MediaRenditionFile(name string, variantB bool) string {
	if variantB {
		return name + ".b.mp4"
	}
	return name + ".mp4"
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

// MediaImageOutput is a variant of an image produced by the worker
// (MediaImageFile(Width, Format) in the sidecar directory, Width 0 for the
// image at its own size).
type MediaImageOutput struct {
	Format string `json:"format"`
	// Width is the width the variant was asked for (0: the image at its
	// own size).
	Width int `json:"width"`
	// PixelWidth and PixelHeight are the size of the variant.
	PixelWidth  int   `json:"pixelWidth"`
	PixelHeight int   `json:"pixelHeight"`
	Size        int64 `json:"size"`
}
