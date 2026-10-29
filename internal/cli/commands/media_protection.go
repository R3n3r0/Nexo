package commands

import (
	"encoding/json"
	"fmt"
	"os"
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
	mode            string
	legacyDevices   string
	forensic        bool
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
  nexoctl media protection set local/courses --mode advanced --forensic
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
	fl.StringVar(&f.mode, "mode", "", "standard (any HLS player) or advanced (only the Nexo player, session keys and segments per session)")
	fl.StringVar(&f.legacyDevices, "legacy-devices", "", "advanced mode, browsers without Media Source Extensions: refuse or standard")
	fl.BoolVar(&f.forensic, "forensic", false, "mark every stream with an invisible watermark naming its playback token")
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
	if changed("mode") {
		p.Mode = strings.ToLower(strings.TrimSpace(f.mode))
	}
	if changed("legacy-devices") {
		p.LegacyDevices = strings.ToLower(strings.TrimSpace(f.legacyDevices))
	}
	if changed("forensic") {
		p.Forensic = f.forensic
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
	fmt.Fprintf(&b, "  Share links:   %s\n", yesNo(&p.ShareLinks))
	mode := "standard (any HLS player)"
	if p.Mode == domain.MediaPlaybackAdvanced {
		mode = "advanced (Nexo player only)"
		if p.LegacyDevices == domain.MediaLegacyStandard {
			mode += ", standard for the browsers without MSE"
		}
	}
	fmt.Fprintf(&b, "  Playback:      %s\n", mode)
	fmt.Fprintf(&b, "  Forensic mark: %s", yesNo(&p.Forensic))
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

// playbackTraceRecord is the result of media trace.
type playbackTraceRecord struct {
	Target string `json:"target"`
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	nexoadmin.PlaybackTrace
}

// traceVotesFile is the output of nexo-media-worker trace (its votes).
type traceVotesFile struct {
	Votes []nexoadmin.PlaybackTraceVote `json:"votes"`
}

func newMediaTraceCmd(e *env) *cobra.Command {
	var (
		versionID, bits, votes string
		limit                  int
	)
	cmd := &cobra.Command{
		Use:   "trace ALIAS/BUCKET/KEY (--votes FILE | --bits BITS)",
		Short: "Find the playback token a leaked copy of a video came from",
		Long: `Find the playback token a leaked copy of a video came from, for a video
protected with the forensic watermark: every stream of a playback token
carries an invisible code, segment by segment. nexo-media-worker trace reads
the copy and writes its votes; this command sends them to Nexo, which reads
the code and lists the tokens whose code agrees, with the viewer they were
issued to (admin:TracePlaybackLeaks). --bits gives the code directly: one
character per segment (0, 1 or ? when unknown), segment n being bit n mod 64.

A full copy names its token with certainty (64 bits); a short clip of a few
segments agrees with many tokens: the more segments, the fewer candidates.`,
		Example: `  nexo-media-worker trace --leak copy.mp4 --bucket courses --key lessons/intro.mp4 --out votes.json
  nexoctl media trace local/courses/lessons/intro.mp4 --votes votes.json
  nexoctl media trace local/courses/lessons/intro.mp4 --bits 0110?1`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (bits == "") == (votes == "") {
				return cli.UsageError("give exactly one of --votes and --bits")
			}
			req := nexoadmin.PlaybackTraceRequest{Bits: bits, Limit: limit}
			if votes != "" {
				data, err := os.ReadFile(votes) // #nosec G304 -- a file the user names
				if err != nil {
					return err
				}
				var f traceVotesFile
				if err := json.Unmarshal(data, &f); err != nil {
					return cli.UsageError("--votes: %s is not the output of nexo-media-worker trace: %v", votes, err)
				}
				if len(f.Votes) == 0 {
					return cli.UsageError("--votes: %s holds no vote", votes)
				}
				req.Votes = f.Votes
			}
			ctx := cmd.Context()
			t, client, err := e.mediaObject(ctx, args[0])
			if err != nil {
				return err
			}
			res, err := client.TracePlaybackLeak(ctx, t.Bucket, t.Key, versionID, req)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Result(playbackTraceRecord{Target: t.String(), Bucket: t.Bucket, Key: t.Key, PlaybackTrace: *res}, traceText(t, res))
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&votes, "votes", "", "the votes of nexo-media-worker trace (a JSON file)")
	fl.StringVar(&bits, "bits", "", "the code read from the copy: 0, 1 or ? per segment")
	fl.IntVar(&limit, "limit", 10, "most candidates listed (1 to 100)")
	fl.StringVar(&versionID, "version-id", "", "version of the video (default: the current one)")
	return cmd
}

// traceText renders the result of media trace.
func traceText(t cli.Target, res *nexoadmin.PlaybackTrace) string {
	known := len(res.Bits) - strings.Count(res.Bits, "?")
	var b strings.Builder
	fmt.Fprintf(&b, "Code read from the copy of `%s`: %s (%d bits known)\n", t, res.Bits, known)
	if len(res.Matches) == 0 {
		b.WriteString("No playback token agrees with it.")
		return b.String()
	}
	fmt.Fprintf(&b, "%-4s %-24s %-32s %-9s %s\n", "#", "SUBJECT", "SESSION", "AGREE", "ISSUED")
	for i, m := range res.Matches {
		fmt.Fprintf(&b, "%-4d %-24s %-32s %-9s %s", i+1, m.Subject, m.SessionID, fmt.Sprintf("%d/%d", m.Agree, m.Compared),
			m.IssuedAt.UTC().Format(time.RFC3339))
		if m.Watermark != "" {
			fmt.Fprintf(&b, "  (%s)", m.Watermark)
		}
		if i < len(res.Matches)-1 {
			b.WriteByte('\n')
		}
	}
	if known < 20 {
		b.WriteString("\nFew bits known: several tokens may agree by chance; a longer copy narrows them down.")
	}
	return b.String()
}
