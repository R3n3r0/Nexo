package commands

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func newMediaCmd(e *env) *cobra.Command {
	cmd := groupCmd("media", "Manage the video and image rules of buckets and the media worker (Admin API)")
	cmd.Long = `Manage the video and image rules of buckets and follow the work of the media
worker.

The rules select videos by key prefix and suffix (default .mp4, .mov and .m4v,
matched without case); the first rule that selects a key applies. For each new
version of a selected video, the nexo-media-worker derives what its rule asks
for: lower-quality renditions (H.264 with the audio of the source; presets
2160p, 1440p, 1080p, 720p, 480p, 360p and 240p, those taller than the source
are skipped), a JPEG poster and timeline thumbnails (a sprite with its WebVTT
map). It writes them in the hidden sidecar prefix ` + domain.MediaSidecarPrefix + ` of the bucket:
ls, find and tree hide it unless --media is given (mirror and cp copy it, so
that the backups keep it). The worker receives its jobs from the notification
target notify_media_worker of the server.

An image rule ("images": {"formats": [...], "widths": [...]}) selects images
instead, by default the suffixes
` + strings.Join(domain.DefaultImageSuffixes(), ", ") + `:
the worker derives from each one a variant in every format (AVIF, WebP, JPEG,
PNG) at every width smaller than the image, and the image at its own size in
every format. The server serves the variant a client asks for, in the most
compact format it accepts ("nexoctl share download --image", the Admin API
and the S3 listener).

The configuration is the JSON document of the Admin API and of the console
({"rules": [{"id": "videos", "prefix": "videos/", "renditions": ["720p"],
"poster": true, "thumbnails": {"interval": "10s"}}, {"id": "images",
"images": {"formats": ["webp", "jpeg"], "widths": [320, 640]}}]}), the form of
"media get". Streaming a video (HLS packaged on the fly) needs no rule.`
	rule := groupCmd("rule", "Add or remove one media rule of a bucket")
	rule.AddCommand(newMediaRuleAddCmd(e), newMediaRuleRemoveCmd(e))
	cmd.AddCommand(newMediaSetCmd(e), newMediaGetCmd(e), newMediaRemoveCmd(e), rule, newMediaReprocessCmd(e), newMediaStatusCmd(e),
		newMediaSubtitlesCmd(e), newMediaProtectionCmd(e), newMediaTokenCmd(e), newMediaRevokeCmd(e))
	return cmd
}

func newMediaSetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "set ALIAS/BUCKET [FILE]",
		Short: "Replace the media rules of a bucket (JSON)",
		Long: `Replace the media rules of a bucket with a JSON document read from FILE or
from the standard input ("-" or no FILE). The rules are checked before they are
sent; the server stores them normalized (default suffixes, renditions tallest
first, thumbnail interval, image formats the most compact first and widths
ascending).`,
		Example: `  nexoctl media set local/videos media.json
  nexoctl media get local/videos | nexoctl media set local/archive`,
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := e.readConfigDocument(args[1:])
			if err != nil {
				return err
			}
			cfg, err := cli.ParseMedia(data)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			stored, err := client.SetBucketMedia(ctx, t.Bucket, cfg)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			n := len(stored.Rules)
			return e.out.Info(mediaRecord{Target: t.String(), Bucket: t.Bucket, Rules: mediaRules(stored)},
				fmt.Sprintf("Media rules of `%s` set (%d %s).", t, n, plural(n, "rule", "rules")))
		},
	}
}

func newMediaGetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "get ALIAS/BUCKET",
		Aliases: []string{"info"},
		Short:   "Print the media rules of a bucket (JSON)",
		Example: `  nexoctl media get local/videos > media.json
  nexoctl media get --json local/videos`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			cfg, err := client.GetBucketMedia(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if e.out.JSON() {
				return e.out.Result(mediaRecord{Target: t.String(), Bucket: t.Bucket, Rules: mediaRules(cfg)}, "")
			}
			if len(cfg.Rules) == 0 {
				return e.out.Result(nil, fmt.Sprintf("`%s` has no media rule.", t))
			}
			doc, err := encodeJSONDocument(nexoadmin.MediaConfiguration{Rules: cfg.Rules})
			if err != nil {
				return err
			}
			_, err = e.out.Stdout().Write(doc)
			return err
		},
	}
}

func newMediaRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "remove ALIAS/BUCKET",
		Aliases: []string{"rm"},
		Short:   "Remove the media rules of a bucket",
		Long: `Remove the media rules of a bucket: the media worker no longer processes its new
videos and images. What it already derived stays in ` + domain.MediaSidecarPrefix + ` and is still served.`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			if err := client.DeleteBucketMedia(ctx, t.Bucket); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(mediaRecord{Target: t.String(), Bucket: t.Bucket, Rules: []nexoadmin.MediaRule{}},
				fmt.Sprintf("Removed the media rules of `%s`.", t))
		},
	}
}

// mediaRuleFlags are the flags of media rule add.
type mediaRuleFlags struct {
	id         string
	prefix     string
	suffixes   []string
	renditions []string
	poster     bool
	thumbnails string
	hls        bool
	images     bool
	formats    []string
	widths     []int
	subtitles  string
	translate  []string
}

// imagesMediaRule is the rule of the images option
// (domain.ImagesMediaRule): every format at every default width.
func imagesMediaRule() nexoadmin.MediaRule {
	return cli.MediaFromDomain(domain.MediaConfig{Rules: []domain.MediaRule{domain.ImagesMediaRule()}}).Rules[0]
}

// imageFormats returns the formats of --format: "jpg" is jpeg.
func imageFormats(list []string) []string {
	out := splitList(list)
	for i, f := range out {
		if f = strings.ToLower(f); f == "jpg" {
			f = "jpeg"
		}
		out[i] = f
	}
	return out
}

// hlsMediaRule is the rule of the HLS option (domain.HLSMediaRule): every
// rendition, a poster and thumbnails.
func hlsMediaRule() nexoadmin.MediaRule {
	return cli.MediaFromDomain(domain.MediaConfig{Rules: []domain.MediaRule{domain.HLSMediaRule()}}).Rules[0]
}

// upsertMediaRule adds rule to the rules of a bucket, or replaces the rule
// with the same ID in its place, and returns the rule as stored.
func upsertMediaRule(ctx context.Context, client cli.AdminClient, bucket string, rule nexoadmin.MediaRule) (stored nexoadmin.MediaRule, replaced bool, err error) {
	cfg, err := client.GetBucketMedia(ctx, bucket)
	if err != nil {
		return rule, false, err
	}
	rules := mediaRules(cfg)
	for i, r := range rules {
		if r.ID == rule.ID {
			rules[i], replaced = rule, true
		}
	}
	if !replaced {
		rules = append(rules, rule)
	}
	out, err := client.SetBucketMedia(ctx, bucket, nexoadmin.MediaConfiguration{Rules: rules})
	if err != nil {
		return rule, false, err
	}
	for _, r := range out.Rules {
		if r.ID == rule.ID {
			rule = r
		}
	}
	return rule, replaced, nil
}

func newMediaRuleAddCmd(e *env) *cobra.Command {
	var f mediaRuleFlags
	cmd := &cobra.Command{
		Use:   "add ALIAS/BUCKET (--id ID | --hls | --images) [flags]",
		Short: "Add a video or image rule to a bucket, or replace the rule with the same ID",
		Long: `Add a video or image rule to a bucket, or replace the rule with the same ID
(keeping its place: the first rule that selects a key applies). A video rule
produces at least one of --rendition, --poster and --thumbnails. --hls is the
rule of the HLS option of bucket creation (id "hls", every rendition, a poster
and thumbnails every 10s); the other flags change it.
Renditions: ` + strings.Join(domain.MediaRenditionNames(), ", ") + ` (those taller than the
source are skipped).

--images makes an image rule (id "images" by default; default suffixes: the
image formats the worker decodes): the worker derives from every image it
selects a variant in every --format (` + strings.Join(domain.MediaImageFormats(), ", ") + `; default all) at every
--width (16 to 8192 pixels; default ` + joinInts(domain.DefaultImageWidths()) + `), widths not
smaller than the image skipped, plus the image at its own size in every format.

--subtitles makes the worker transcribe the speech of the videos (the language
given, or detected with "auto" or no value) into a subtitle track, and
--translate adds a translated track per language (BCP 47 tags: it, en,
pt-BR...). The worker needs a transcription service ("media subtitles").`,
		Example: `  nexoctl media rule add local/videos --id hd --prefix courses/ --rendition 720p,480p --poster
  nexoctl media rule add local/videos --id clips --suffix .mp4 --suffix .webm --thumbnails 5s
  nexoctl media rule add local/videos --hls
  nexoctl media rule add local/photos --images
  nexoctl media rule add local/videos --id talks --prefix talks/ --subtitles=it --translate en,de
  nexoctl media rule add local/photos --images --id thumbs --prefix gallery/ --format avif,webp --width 320,640`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			changed := cmd.Flags().Changed
			rule := nexoadmin.MediaRule{ID: strings.TrimSpace(f.id), Prefix: f.prefix, Suffixes: splitList(f.suffixes),
				Renditions: splitList(f.renditions), Poster: f.poster}
			if changed("thumbnails") {
				rule.Thumbnails = &nexoadmin.MediaThumbnails{Interval: f.thumbnails}
			}
			if changed("subtitles") || changed("translate") {
				rule.Subtitles = &nexoadmin.MediaSubtitles{Source: f.subtitles, Languages: splitList(f.translate)}
				if rule.Subtitles.Source == "auto" {
					rule.Subtitles.Source = ""
				}
			}
			if f.hls && f.images {
				return cli.UsageError("--hls and --images are two rules: add them one at a time")
			}
			if !f.images && (changed("format") || changed("width")) {
				return cli.UsageError("--format and --width apply to an image rule (--images)")
			}
			if f.images {
				preset := imagesMediaRule()
				if rule.ID == "" {
					rule.ID = preset.ID
				}
				rule.Images = preset.Images
				if changed("format") {
					rule.Images.Formats = imageFormats(f.formats)
				}
				if changed("width") {
					rule.Images.Widths = f.widths
				}
			}
			if f.hls {
				preset := hlsMediaRule()
				if rule.ID == "" {
					rule.ID = preset.ID
				}
				if !changed("rendition") {
					rule.Renditions = preset.Renditions
				}
				if !changed("poster") {
					rule.Poster = preset.Poster
				}
				if !changed("thumbnails") {
					rule.Thumbnails = preset.Thumbnails
				}
			}
			if rule.ID == "" {
				return cli.UsageError("--id is required (or --hls, --images)")
			}
			if _, err := cli.NormalizeMedia(nexoadmin.MediaConfiguration{Rules: []nexoadmin.MediaRule{rule}}); err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			rule, replaced, err := upsertMediaRule(ctx, client, t.Bucket, rule)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			text := fmt.Sprintf("Media rule %q added to `%s`.", rule.ID, t)
			if replaced {
				text = fmt.Sprintf("Media rule %q of `%s` replaced.", rule.ID, t)
			}
			return e.out.Info(mediaRuleRecord{Target: t.String(), Bucket: t.Bucket, MediaRule: rule}, text)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.id, "id", "", "ID of the rule (1-64 letters, digits, '.', '_' or '-'; required)")
	fs.StringVar(&f.prefix, "prefix", "", "select the keys under this prefix (default: the whole bucket)")
	fs.StringSliceVar(&f.suffixes, "suffix", nil, "select the keys with these suffixes, without case (repeatable or comma-separated; default .mp4,.mov,.m4v)")
	fs.StringSliceVar(&f.renditions, "rendition", nil, "produce these renditions (repeatable or comma-separated: "+strings.Join(domain.MediaRenditionNames(), ",")+")")
	fs.BoolVar(&f.poster, "poster", false, "produce a JPEG poster frame")
	fs.StringVar(&f.thumbnails, "thumbnails", "", "produce timeline thumbnails, one every INTERVAL (1s to 10m, e.g. 10s)")
	fs.BoolVar(&f.hls, "hls", false, `the rule of the HLS option: id "hls", every rendition, a poster and thumbnails`)
	fs.BoolVar(&f.images, "images", false, `an image rule (default id "images"): the variants of every image`)
	fs.StringSliceVar(&f.formats, "format", nil, "image rule: the formats of the variants (repeatable or comma-separated: "+strings.Join(domain.MediaImageFormats(), ",")+"; default all)")
	fs.StringVar(&f.subtitles, "subtitles", "", `automatic subtitles: the spoken LANG transcribed ("auto" detects it)`)
	fs.Lookup("subtitles").NoOptDefVal = "auto"
	fs.StringSliceVar(&f.translate, "translate", nil, "automatic subtitles: translate the transcription in these languages (repeatable or comma-separated)")
	fs.IntSliceVar(&f.widths, "width", nil, "image rule: the widths of the variants in pixels (repeatable or comma-separated; default "+joinInts(domain.DefaultImageWidths())+")")
	return cmd
}

func newMediaRuleRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS/BUCKET ID",
		Aliases:           []string{"rm"},
		Short:             "Remove a media rule of a bucket",
		Example:           "  nexoctl media rule rm local/videos hd",
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			cfg, err := client.GetBucketMedia(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			var (
				kept    []nexoadmin.MediaRule
				removed *nexoadmin.MediaRule
			)
			for _, r := range cfg.Rules {
				if r.ID == args[1] {
					removed = &r
					continue
				}
				kept = append(kept, r)
			}
			if removed == nil {
				return fmt.Errorf("%s: media rule %q: %w", t, args[1], domain.ErrNotFound)
			}
			if len(kept) == 0 {
				err = client.DeleteBucketMedia(ctx, t.Bucket)
			} else {
				_, err = client.SetBucketMedia(ctx, t.Bucket, nexoadmin.MediaConfiguration{Rules: kept})
			}
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(mediaRuleRecord{Target: t.String(), Bucket: t.Bucket, MediaRule: *removed},
				fmt.Sprintf("Media rule %q removed from `%s`.", removed.ID, t))
		},
	}
}

// mediaObject resolves ALIAS/BUCKET/KEY and the Admin API client of the
// alias.
func (e *env) mediaObject(ctx context.Context, arg string) (cli.Target, cli.AdminClient, error) {
	t, err := e.remoteTarget(ctx, arg, true)
	if err != nil {
		return cli.Target{}, nil, err
	}
	if t.Key == "" || strings.HasSuffix(t.Key, "/") {
		return cli.Target{}, nil, cli.UsageError("%s: expected a video or an image (ALIAS/BUCKET/KEY)", arg)
	}
	client, err := e.admin(ctx, t.Alias)
	if err != nil {
		return cli.Target{}, nil, err
	}
	return t, client, nil
}

func newMediaReprocessCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:   "reprocess ALIAS/BUCKET/KEY [--version-id V]",
		Short: "Queue the processing of a video or an image again",
		Long: `Queue a job of the media worker for a video or an image (default its latest
version), with the rule of its bucket that selects it now: after a change of the
rules, or to retry a failed job. "media status" follows the job.`,
		Example: `  nexoctl media reprocess local/videos/courses/intro.mp4
  nexoctl media reprocess --version-id 3f2a... local/videos/courses/intro.mp4`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.ReprocessMedia(ctx, t.Bucket, t.Key, versionID); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(mediaJobRecord{Target: t.String(), Bucket: t.Bucket, Key: t.Key, VersionID: versionID},
				fmt.Sprintf("Processing of `%s`%s queued.", t, versionText(versionID)))
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "the version to process (default: the latest)")
	return cmd
}

func newMediaStatusCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:   "status ALIAS/BUCKET/KEY [--version-id V]",
		Short: "Show the processing state of a video or an image by the media worker",
		Long: `Show the processing state of a video or an image (default its latest version):
the state of its job (none, processing, done or failed), the step in progress or
that failed, the rule that selected it, the source and what the worker derived
(the renditions, poster and thumbnails of a video, the variants of an image).
The outputs are current while they derive from the content of the version;
stale outputs (the object was overwritten since) are not served.`,
		Example: `  nexoctl media status local/videos/courses/intro.mp4
  nexoctl media status --json local/videos/courses/intro.mp4`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			st, err := client.GetMediaStatus(ctx, t.Bucket, t.Key, versionID)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Result(mediaStatusRecord{Target: t.String(), Bucket: t.Bucket, Key: t.Key, MediaStatus: *st}, e.mediaStatusText(t, st))
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "the version (default: the latest)")
	return cmd
}

// versionText names a version in a message ("" for the latest).
func versionText(versionID string) string {
	if versionID == "" {
		return ""
	}
	return " (version " + versionID + ")"
}

// mediaStatusText renders the processing state of a video.
func (e *env) mediaStatusText(t cli.Target, st *nexoadmin.MediaStatus) string {
	var b bytes.Buffer
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%-11s: %s\n", k, v)
		}
	}
	if len(st.Images) > 0 || isImageKey(t.Key) {
		row("Image", t.String())
	} else {
		row("Video", t.String())
	}
	row("VersionID", st.VersionID)
	state := st.State
	switch {
	case st.State == "none":
		state = "none (never processed)"
	case st.State == "done" && st.Current:
		state = e.out.Paint(cli.ColorGreen, "done") + " (current)"
	case st.State == "done":
		state = e.out.Paint(cli.ColorYellow, "done") + " (stale: the object changed since, not served)"
	case st.State == "failed":
		state = e.out.Paint(cli.ColorRed, "failed")
	}
	row("State", state)
	row("Step", st.Step)
	row("Error", st.Error)
	row("Rule", st.Rule)
	if st.Width > 0 || st.Duration > 0 {
		var src []string
		if st.Width > 0 {
			src = append(src, fmt.Sprintf("%dx%d", st.Width, st.Height))
		}
		if st.Duration > 0 {
			src = append(src, time.Duration(st.Duration*float64(time.Second)).Round(100*time.Millisecond).String())
		}
		row("Source", strings.Join(src, ", "))
	}
	if len(st.Renditions) > 0 {
		var rs []string
		for _, r := range st.Renditions {
			rs = append(rs, fmt.Sprintf("%s %dx%d %s", r.Name, r.Width, r.Height, bitrateText(r.Bandwidth)))
		}
		row("Renditions", strings.Join(rs, ", "))
	}
	if st.Poster {
		row("Poster", "yes")
	}
	if th := st.Thumbnails; th != nil {
		interval := time.Duration(th.Interval * float64(time.Second))
		row("Thumbnails", fmt.Sprintf("%d every %s (%dx%d, %d %s)", th.Count, interval, th.Width, th.Height,
			th.Columns, plural(th.Columns, "column", "columns")))
	}
	if len(st.Images) > 0 {
		row("Images", imageVariantsText(st.Images))
	}
	row("Language", st.Language)
	if len(st.Subtitles) > 0 {
		row("Subtitles", strings.Join(st.Subtitles, ", "))
	}
	if st.SubtitlesError != "" {
		row("Subtitles", e.out.Paint(cli.ColorRed, "failed")+": "+st.SubtitlesError)
	}
	if st.Updated != nil {
		row("Updated", cli.FormatTime(*st.Updated))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// imageVariantsText renders the variants of an image, one line per format
// ("webp: 320x240 1.2 KiB, 640x480 4.0 KiB, full 1000x750 9.8 KiB"), the
// formats in the order of the worker, the widths ascending.
func imageVariantsText(variants []nexoadmin.MediaImageOutput) string {
	byFormat := map[string][]nexoadmin.MediaImageOutput{}
	var formats []string
	for _, v := range variants {
		if _, ok := byFormat[v.Format]; !ok {
			formats = append(formats, v.Format)
		}
		byFormat[v.Format] = append(byFormat[v.Format], v)
	}
	lines := make([]string, 0, len(formats))
	for _, f := range formats {
		vs := byFormat[f]
		sort.SliceStable(vs, func(i, j int) bool {
			// The image at its own size last.
			if (vs[i].Width == 0) != (vs[j].Width == 0) {
				return vs[j].Width == 0
			}
			return vs[i].PixelWidth < vs[j].PixelWidth
		})
		parts := make([]string, 0, len(vs))
		for _, v := range vs {
			size := fmt.Sprintf("%dx%d %s", v.PixelWidth, v.PixelHeight, cli.HumanSize(v.Size))
			if v.Width == 0 {
				size = "full " + size
			}
			parts = append(parts, size)
		}
		lines = append(lines, f+": "+strings.Join(parts, ", "))
	}
	return strings.Join(lines, "\n             ")
}

// bitrateText renders a bandwidth in bits per second.
func bitrateText(bps int) string {
	if bps >= 1_000_000 {
		return fmt.Sprintf("%.1f Mbit/s", float64(bps)/1e6)
	}
	return fmt.Sprintf("%d kbit/s", bps/1000)
}

// mediaRules returns the rules of a configuration, never nil.
func mediaRules(cfg *nexoadmin.MediaConfiguration) []nexoadmin.MediaRule {
	if cfg == nil || cfg.Rules == nil {
		return []nexoadmin.MediaRule{}
	}
	return append([]nexoadmin.MediaRule(nil), cfg.Rules...)
}

// joinInts renders numbers separated by commas.
func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// isImageKey reports a key with a suffix of the image rules
// (domain.DefaultImageSuffixes).
func isImageKey(key string) bool {
	lower := strings.ToLower(key)
	for _, suffix := range domain.DefaultImageSuffixes() {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}
