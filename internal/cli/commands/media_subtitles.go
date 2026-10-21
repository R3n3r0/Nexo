package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func newMediaSubtitlesCmd(e *env) *cobra.Command {
	cmd := groupCmd("subtitles", "List, download, upload or remove the subtitle tracks of a video")
	cmd.Aliases = []string{"subs"}
	cmd.Long = `List, download, upload or remove the subtitle tracks of a video (default its
latest version).

A track is manual (uploaded, or edited in the console) or automatic: a media
rule with "subtitles" makes the media worker transcribe the speech of the video
and translate the transcription. An automatic track applies only to the content
it derives from (the video overwritten, it is no longer served until the worker
runs again); a manual track always applies and the worker never overwrites it.
The master playlist of the video lists the tracks: a player shows them as
captions.`
	cmd.AddCommand(newMediaSubtitlesListCmd(e), newMediaSubtitlesGetCmd(e), newMediaSubtitlesPutCmd(e), newMediaSubtitlesRemoveCmd(e))
	return cmd
}

// subtitleTrackRecord is a subtitle track of a video.
type subtitleTrackRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	nexoadmin.SubtitleTrack
}

// subtitlesRecord lists the subtitle tracks of a video.
type subtitlesRecord struct {
	Target string                    `json:"target"`
	Bucket string                    `json:"bucket"`
	Key    string                    `json:"key"`
	Tracks []nexoadmin.SubtitleTrack `json:"tracks"`
}

// subtitleOriginText describes how a track was made.
func subtitleOriginText(t nexoadmin.SubtitleTrack) string {
	switch {
	case t.Auto && t.From != "":
		return "translated from " + t.From
	case t.Auto:
		return "transcribed"
	case t.UpdatedBy != "":
		return "manual (" + t.UpdatedBy + ")"
	default:
		return "manual"
	}
}

func newMediaSubtitlesListCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:               "ls ALIAS/BUCKET/KEY [--version-id V]",
		Aliases:           []string{"list"},
		Short:             "List the subtitle tracks of a video",
		Example:           "  nexoctl media subtitles ls local/videos/courses/intro.mp4",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			tracks, err := client.ListSubtitles(ctx, t.Bucket, t.Key, versionID)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if tracks == nil {
				tracks = []nexoadmin.SubtitleTrack{}
			}
			var b strings.Builder
			if len(tracks) == 0 {
				fmt.Fprintf(&b, "No subtitles for `%s`.", t)
			}
			for i, tr := range tracks {
				if i > 0 {
					b.WriteByte('\n')
				}
				updated := ""
				if tr.Updated != nil {
					updated = cli.FormatTime(*tr.Updated)
				}
				b.WriteString(strings.TrimRight(fmt.Sprintf("%-8s %-14s %5d %-5s %9s  %-26s %s", tr.Lang, tr.Name, tr.Cues,
					plural(tr.Cues, "cue", "cues"), cli.HumanSize(tr.Size), subtitleOriginText(tr), updated), " "))
			}
			return e.out.Result(subtitlesRecord{Target: t.String(), Bucket: t.Bucket, Key: t.Key, Tracks: tracks}, b.String())
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "the version (default: the latest)")
	return cmd
}

func newMediaSubtitlesGetCmd(e *env) *cobra.Command {
	var versionID, format, output string
	cmd := &cobra.Command{
		Use:   "get ALIAS/BUCKET/KEY LANG [--format vtt|srt] [-o FILE]",
		Short: "Download a subtitle track (WebVTT or SubRip)",
		Example: `  nexoctl media subtitles get local/videos/courses/intro.mp4 it > intro.it.vtt
  nexoctl media subtitles get --format srt -o intro.en.srt local/videos/courses/intro.mp4 en`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "vtt" && format != "srt" {
				return cli.UsageError("--format must be vtt or srt")
			}
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			doc, err := client.ExportSubtitles(ctx, t.Bucket, t.Key, versionID, args[1], format)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if output == "" || output == "-" {
				_, err = e.out.Stdout().Write(doc)
				return err
			}
			if err := os.WriteFile(output, doc, 0o600); err != nil {
				return err
			}
			return e.out.Note(fmt.Sprintf("Subtitles %s of `%s` saved to %s.", args[1], t, output))
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&versionID, "version-id", "", "the version (default: the latest)")
	fs.StringVar(&format, "format", "vtt", "the format of the document: vtt (WebVTT) or srt (SubRip)")
	fs.StringVarP(&output, "output", "o", "", "write the document to FILE (default: the standard output)")
	return cmd
}

func newMediaSubtitlesPutCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:   "put ALIAS/BUCKET/KEY LANG [FILE]",
		Short: "Upload a subtitle track from a WebVTT or SubRip file",
		Long: `Upload a WebVTT or SubRip document (FILE, or the standard input with "-" or no
FILE; at most 2 MiB) as the manual subtitle track of a language (a BCP 47 tag:
it, en, pt-BR...), replacing the track of that language: an automatic one too,
which the media worker then leaves alone. The text of the cues is sanitized:
only the WebVTT tags b, i, u, c, v, lang and ruby are kept.`,
		Example: `  nexoctl media subtitles put local/videos/courses/intro.mp4 it intro.it.srt
  nexoctl media subtitles get local/videos/a.mp4 en | nexoctl media subtitles put local/videos/b.mp4 en`,
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 2, 3) },
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			lang, err := domain.CanonicalLanguage(args[1])
			if err != nil {
				return cli.UsageError("%s: not a language tag (it, en, pt-BR...)", args[1])
			}
			in := e.deps.Stdin
			if len(args) == 3 && args[2] != "-" {
				f, err := os.Open(args[2])
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				in = f
			}
			doc, err := io.ReadAll(io.LimitReader(in, domain.MaxSubtitleBytes+1))
			if err != nil {
				return err
			}
			cues, err := domain.ParseSubtitles(doc)
			if err != nil {
				return cli.UsageError("%s", strings.TrimPrefix(err.Error(), domain.ErrInvalidArgument.Error()+": "))
			}
			if len(cues) == 0 {
				return cli.UsageError("the document has no cue")
			}
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			tr, err := client.ImportSubtitles(ctx, t.Bucket, t.Key, versionID, lang, doc)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(subtitleTrackRecord{Target: t.String(), Bucket: t.Bucket, Key: t.Key, SubtitleTrack: *tr},
				fmt.Sprintf("Subtitles %s of `%s` uploaded (%d %s).", tr.Lang, t, tr.Cues, plural(tr.Cues, "cue", "cues")))
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "the version (default: the latest)")
	return cmd
}

func newMediaSubtitlesRemoveCmd(e *env) *cobra.Command {
	var versionID string
	cmd := &cobra.Command{
		Use:     "rm ALIAS/BUCKET/KEY LANG [--version-id V]",
		Aliases: []string{"remove"},
		Short:   "Remove a subtitle track of a video",
		Long: `Remove the subtitle track of a language. An automatic track comes back when the
media worker processes the video again ("media reprocess").`,
		Example:           "  nexoctl media subtitles rm local/videos/courses/intro.mp4 de",
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.DeleteSubtitles(ctx, t.Bucket, t.Key, versionID, args[1]); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(subtitleTrackRecord{Target: t.String(), Bucket: t.Bucket, Key: t.Key, SubtitleTrack: nexoadmin.SubtitleTrack{Lang: args[1]}},
				fmt.Sprintf("Subtitles %s of `%s` removed.", args[1], t))
		},
	}
	cmd.Flags().StringVar(&versionID, "version-id", "", "the version (default: the latest)")
	return cmd
}
