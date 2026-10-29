package commands

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// mediaProtectionRecord is the download protection of the videos of a
// bucket (media protection get, set).
type mediaProtectionRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	nexoadmin.MediaProtection
}

// playbackTokenRecord is a playback token (media token).
type playbackTokenRecord struct {
	Target    string `json:"target"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
	Subject   string `json:"subject"`
	nexoadmin.PlaybackToken
}

// playbackRevocationRecord is a revocation (media revoke).
type playbackRevocationRecord struct {
	Target    string `json:"target"`
	Bucket    string `json:"bucket"`
	SessionID string `json:"sessionId,omitempty"`
	Subject   string `json:"subject,omitempty"`
}

func newMediaProtectionCmd(e *env) *cobra.Command {
	cmd := groupCmd("protection", "Show or change the download protection of the videos of a bucket")
	cmd.Long = `Show or change the download protection of the videos of a bucket.

A protected video streams encrypted (HLS AES-128, a key every 10 segments): its
keys go only to a playback token of the video ("nexoctl media token") or to a
console session or a signed request allowed to stream it, at most PACE times
faster than the video plays and to MAX-SESSIONS simultaneous streams of a
viewer (a newer stream replaces the oldest). Its original cannot be downloaded
without nexo:DownloadProtectedMedia. The Nexo player draws the viewer's
watermark over the picture.`
	cmd.AddCommand(newMediaProtectionGetCmd(e), newMediaProtectionSetCmd(e))
	return cmd
}

func newMediaProtectionGetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "get ALIAS/BUCKET",
		Aliases:           []string{"info"},
		Short:             "Show the download protection of the videos of a bucket",
		Example:           `  nexoctl media protection get local/courses`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			p, err := client.GetMediaProtection(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Result(mediaProtectionRecord{Target: t.String(), Bucket: t.Bucket, MediaProtection: *p}, mediaProtectionText(t, p))
		},
	}
}

// mediaProtectionFlags are the flags of media protection set.
type mediaProtectionFlags struct {
	enable, disable bool
	prefix          string
	suffixes        []string
	tokenTTL        time.Duration
	maxSessions     string
	pace            int
	watermark       bool
	origins         []string
	shareLinks      bool
}

func newMediaProtectionSetCmd(e *env) *cobra.Command {
	var f mediaProtectionFlags
	cmd := &cobra.Command{
		Use:   "set ALIAS/BUCKET [flags]",
		Short: "Change the download protection of the videos of a bucket",
		Long: `Change the download protection of the videos of a bucket: the flags given
replace those settings, the others keep their value.`,
		Example: `  nexoctl media protection set local/courses --enable --prefix lessons/
  nexoctl media protection set local/courses --max-sessions 1 --origin https://learn.example.com
  nexoctl media protection set local/courses --max-sessions unlimited --token-ttl 4h
  nexoctl media protection set local/courses --disable`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.enable && f.disable {
				return cli.UsageError("--enable and --disable are exclusive")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			p, err := client.GetMediaProtection(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if err := f.apply(cmd, p); err != nil {
				return err
			}
			if err := checkMediaProtection(*p); err != nil {
				return err
			}
			stored, err := client.SetMediaProtection(ctx, t.Bucket, *p)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			state := "disabled"
			if stored.Enabled {
				state = "enabled"
			}
			return e.out.Info(mediaProtectionRecord{Target: t.String(), Bucket: t.Bucket, MediaProtection: *stored},
				fmt.Sprintf("Video protection of `%s` %s.", t, state))
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.enable, "enable", false, "protect the videos")
	fl.BoolVar(&f.disable, "disable", false, "stop protecting the videos")
	fl.StringVar(&f.prefix, "prefix", "", "protect only the keys under this prefix (\"\" for the whole bucket)")
	fl.StringSliceVar(&f.suffixes, "suffix", nil, "suffixes of the protected videos (repeatable; default .mp4, .mov, .m4v)")
	fl.DurationVar(&f.tokenTTL, "token-ttl", 0, "longest validity of a playback token (1m to 24h)")
	fl.StringVar(&f.maxSessions, "max-sessions", "", "simultaneous streams of a viewer (1 to 100, or unlimited)")
	fl.IntVar(&f.pace, "pace", 0, "keys served at most this many times faster than the video plays (1 to 20)")
	fl.BoolVar(&f.watermark, "watermark", true, "draw the viewer's watermark in the Nexo player")
	fl.StringSliceVar(&f.origins, "origin", nil, "web origin allowed to embed the player and fetch the keys (repeatable; \"\" clears)")
	fl.BoolVar(&f.shareLinks, "share-links", false, "let share links play the protected videos (without watermark nor limits)")
	return cmd
}

// apply sets the settings of the flags given on p.
func (f *mediaProtectionFlags) apply(cmd *cobra.Command, p *nexoadmin.MediaProtection) error {
	changed := cmd.Flags().Changed
	switch {
	case f.enable:
		p.Enabled = true
	case f.disable:
		p.Enabled = false
	}
	if changed("prefix") {
		p.Prefix = f.prefix
	}
	if changed("suffix") {
		p.Suffixes = f.suffixes
	}
	if changed("token-ttl") {
		if f.tokenTTL%time.Second != 0 {
			return cli.UsageError("--token-ttl: whole seconds only")
		}
		p.TokenTTLSeconds = int(f.tokenTTL / time.Second)
	}
	if changed("max-sessions") {
		n, err := parseMaxSessions(f.maxSessions)
		if err != nil {
			return err
		}
		p.MaxSessions = n
	}
	if changed("pace") {
		p.PaceFactor = f.pace
	}
	if changed("watermark") {
		p.Watermark = f.watermark
	}
	if changed("origin") {
		p.AllowedOrigins = []string{}
		for _, o := range f.origins {
			if o = strings.TrimSpace(o); o != "" {
				p.AllowedOrigins = append(p.AllowedOrigins, o)
			}
		}
	}
	if changed("share-links") {
		p.ShareLinks = f.shareLinks
	}
	return nil
}

// parseMaxSessions reads --max-sessions: a number or unlimited (-1).
func parseMaxSessions(v string) (int, error) {
	if strings.EqualFold(v, "unlimited") {
		return -1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, cli.UsageError("--max-sessions: expected 1 to 100 or unlimited, got %q", v)
	}
	return n, nil
}

// checkMediaProtection validates a protection before it is sent, as the
// server does.
func checkMediaProtection(p nexoadmin.MediaProtection) error {
	d := cli.MediaProtectionDomain(p)
	if err := d.Validate(); err != nil {
		return cli.UsageError("%s", strings.TrimPrefix(err.Error(), domain.ErrInvalidArgument.Error()+": "))
	}
	return nil
}

// mediaProtectionText renders a protection.
func mediaProtectionText(t cli.Target, p *nexoadmin.MediaProtection) string {
	if !p.Enabled {
		return fmt.Sprintf("Video protection of `%s`: disabled.", t)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Video protection of `%s`: enabled\n", t)
	scope := "the whole bucket"
	if p.Prefix != "" {
		scope = p.Prefix
	}
	fmt.Fprintf(&b, "  Videos:        %s (%s)\n", scope, strings.Join(p.Suffixes, ", "))
	fmt.Fprintf(&b, "  Token TTL:     %s\n", time.Duration(p.TokenTTLSeconds)*time.Second)
	sessions := strconv.Itoa(p.MaxSessions)
	if p.MaxSessions < 0 {
		sessions = "unlimited"
	}
	fmt.Fprintf(&b, "  Streams:       %s per viewer\n", sessions)
	fmt.Fprintf(&b, "  Pace:          %dx playback\n", p.PaceFactor)
	fmt.Fprintf(&b, "  Watermark:     %s\n", yesNo(&p.Watermark))
	origins := "any"
	if len(p.AllowedOrigins) > 0 {
		origins = strings.Join(p.AllowedOrigins, ", ")
	}
	fmt.Fprintf(&b, "  Origins:       %s\n", origins)
	fmt.Fprintf(&b, "  Share links:   %s", yesNo(&p.ShareLinks))
	return b.String()
}

func newMediaTokenCmd(e *env) *cobra.Command {
	var (
		versionID, subject, watermark string
		ttl                           time.Duration
	)
	cmd := &cobra.Command{
		Use:   "token ALIAS/BUCKET/KEY --subject VIEWER [--watermark TEXT] [--ttl D]",
		Short: "Issue a playback token of a video for a viewer",
		Long: `Issue a playback token of a video for a viewer: a capability to stream the
video for a while without credentials, which an application hands to its user.
It prints the master playlist with the token, for any HLS player (hls.js,
Safari, ExoPlayer, VLC...), and the Nexo player page. The keys of a protected
video go to the token at the pace of playback, to at most the simultaneous
streams of its subject; "nexoctl media revoke" stops it.`,
		Example: `  nexoctl media token local/courses/lessons/intro.mp4 --subject student-1842 --watermark mario.rossi@example.com
  nexoctl media token --json --ttl 30m local/courses/lessons/intro.mp4 --subject student-1842`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(subject) == "" {
				return cli.UsageError("--subject is required: the viewer the token is for")
			}
			if ttl%time.Second != 0 || ttl < 0 {
				return cli.UsageError("--ttl: a positive number of whole seconds")
			}
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			tok, err := client.IssuePlaybackToken(ctx, t.Bucket, t.Key, versionID,
				nexoadmin.PlaybackTokenRequest{Subject: subject, Watermark: watermark, TTLSeconds: int(ttl / time.Second)})
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			text := fmt.Sprintf("Playback token of `%s` for %s, valid until %s (session %s).\nPlaylist: %s\nPlayer:   %s",
				t, subject, tok.ExpiresAt.UTC().Format(time.RFC3339), tok.SessionID, tok.Playlist, tok.Player)
			return e.out.Result(playbackTokenRecord{Target: t.String(), Bucket: t.Bucket, Key: t.Key, VersionID: versionID, Subject: subject,
				PlaybackToken: *tok}, text)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&subject, "subject", "", "the viewer the token is for (an account id of the application)")
	fl.StringVar(&watermark, "watermark", "", "text the Nexo player draws over the video (default: the subject)")
	fl.DurationVar(&ttl, "ttl", 0, "validity of the token (default 2h, at most the token TTL of the protection)")
	fl.StringVar(&versionID, "version-id", "", "pin the token to a version (default: the current one)")
	return cmd
}

func newMediaRevokeCmd(e *env) *cobra.Command {
	var session, subject string
	cmd := &cobra.Command{
		Use:   "revoke ALIAS/BUCKET (--session ID | --subject VIEWER)",
		Short: "Revoke a playback token, or every token of a viewer",
		Long: `Revoke a playback token by its session id, or every token of the videos of the
bucket issued to a viewer so far: they stop playing on every node within
seconds. A token issued to the viewer afterwards plays.`,
		Example: `  nexoctl media revoke local/courses --session 9f2c4e...
  nexoctl media revoke local/courses --subject student-1842`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (session == "") == (subject == "") {
				return cli.UsageError("give exactly one of --session and --subject")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "media")
			if err != nil {
				return err
			}
			rec := playbackRevocationRecord{Target: t.String(), Bucket: t.Bucket, SessionID: session, Subject: subject}
			if session != "" {
				if err := client.RevokePlaybackSession(ctx, t.Bucket, session); err != nil {
					return fmt.Errorf("%s: %w", t, err)
				}
				return e.out.Info(rec, fmt.Sprintf("Playback token %s of `%s` revoked.", session, t))
			}
			if err := client.RevokePlaybackSubject(ctx, t.Bucket, subject); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(rec, fmt.Sprintf("Playback tokens of %s for `%s` revoked.", subject, t))
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "the session id of the token")
	cmd.Flags().StringVar(&subject, "subject", "", "the viewer whose tokens are revoked")
	return cmd
}
