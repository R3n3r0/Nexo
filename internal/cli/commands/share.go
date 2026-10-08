package commands

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// defaultShareExpiry is the validity of the share links (the maximum of
// SigV4).
const defaultShareExpiry = "7d"

func newShareCmd(e *env) *cobra.Command {
	cmd := groupCmd("share", "Share objects with links that need no credentials")
	cmd.Long = `Share objects with download and upload links that need no credentials.

By default the links are managed: the Nexo server signs each link with a
dedicated service account, lists it ("share list") and revokes it on demand
("share revoke"), with immediate effect. --raw signs the URL locally with
the credentials of the alias instead (any S3 endpoint): a raw URL cannot be
revoked one by one, only by removing the access key or the permissions of its
user. POST policies for HTML forms (share upload --post) are always signed
locally. Every link expires after --expire (at most 7 days).`
	cmd.AddCommand(newShareDownloadCmd(e), newShareUploadCmd(e), newShareListCmd(e), newShareRevokeCmd(e))
	return cmd
}

// shareMode reads --managed and --raw: managed links unless --raw.
func shareMode(cmd *cobra.Command) (managed bool, err error) {
	f := cmd.Flags()
	raw, _ := f.GetBool("raw")
	managed, _ = f.GetBool("managed")
	if raw && f.Changed("managed") && managed {
		return false, cli.UsageError("--managed excludes --raw")
	}
	return managed && !raw, nil
}

// shareModeFlags adds --managed and --raw.
func shareModeFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("managed", true, "managed, revocable link signed by the server (Admin API)")
	cmd.Flags().Bool("raw", false, "raw URL signed locally with the alias credentials (not revocable)")
}

func newShareDownloadCmd(e *env) *cobra.Command {
	var (
		expire, versionID, disposition, contentType string
		recursive                                   bool
	)
	cmd := &cobra.Command{
		Use:   "download [flags] ALIAS/BUCKET/OBJECT",
		Short: "Generate download links (managed by default, --raw for presigned GET)",
		Example: `  nexoctl share download --expire 2h local/docs/report.pdf
  nexoctl share download --recursive local/docs/2026/
  nexoctl share download --raw --expire 10m local/docs/report.pdf`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			managed, err := shareMode(cmd)
			if err != nil {
				return err
			}
			expiry, err := parseExpiry(expire)
			if err != nil {
				return err
			}
			t, err := e.remoteTarget(ctx, args[0], true)
			if err != nil {
				return err
			}
			keys := []string{t.Key}
			if recursive {
				if versionID != "" {
					return cli.UsageError("--version-id excludes --recursive")
				}
				keys = nil
				for en, err := range cli.Walk(ctx, e.resolver(), t) {
					if err != nil {
						return err
					}
					keys = append(keys, cli.DirPrefix(t.Key)+en.Rel)
				}
			} else if t.Key == "" || strings.HasSuffix(t.Key, "/") {
				return cli.UsageError("%s is a bucket or a prefix: use --recursive", t)
			}
			if managed {
				admin, err := e.admin(ctx, t.Alias)
				if err != nil {
					return err
				}
				for _, key := range keys {
					sh, err := admin.CreateShare(ctx, nexoadmin.CreateShareRequest{
						Bucket: t.Bucket, Key: key, Permission: nexoadmin.PermissionDownload, ExpiresIn: int64(expiry / time.Second),
						ContentType: contentType, ContentDisposition: disposition, VersionID: versionID,
					})
					if err != nil {
						return fmt.Errorf("%s: %w", t.Child(key), err)
					}
					if err := e.printManagedShare(t.Child(key), sh, http.MethodGet, nil, expiry, ""); err != nil {
						return err
					}
				}
				return nil
			}
			client, err := e.client(ctx, t.Alias)
			if err != nil {
				return err
			}
			for _, key := range keys {
				req, err := client.Presign(ctx, cli.PresignInput{
					Method: http.MethodGet, Bucket: t.Bucket, Key: key, VersionID: versionID, Expiry: expiry,
					ResponseContentDisposition: disposition, ResponseContentType: contentType,
				})
				if err != nil {
					return fmt.Errorf("%s: %w", t.Child(key), err)
				}
				if err := e.printShare(t.Child(key), req, versionID, expiry, ""); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&expire, "expire", "E", defaultShareExpiry, "validity of the links (1s to 7d)")
	f.StringVar(&versionID, "version-id", "", "share this version of the object")
	f.StringVar(&disposition, "content-disposition", "", `Content-Disposition of the download (e.g. "attachment")`)
	f.StringVar(&contentType, "content-type", "", "Content-Type of the download")
	f.BoolVarP(&recursive, "recursive", "r", false, "one link per object under the prefix")
	shareModeFlags(cmd)
	return cmd
}

func newShareUploadCmd(e *env) *cobra.Command {
	var (
		expire, contentType, maxSize, minSize, prefix string
		post                                          bool
	)
	cmd := &cobra.Command{
		Use:   "upload [flags] ALIAS/BUCKET/OBJECT",
		Short: "Generate upload links (managed PUT by default, --raw, or a --post policy)",
		Long: `Generate an upload link for an object (a PUT URL, managed by default) with the
curl command that uses it, or with --post a POST policy for HTML forms, signed
locally: the form fields, a curl command and an HTML form. A POST policy
targets a key, or with --prefix (or a target ending with "/") any key under a
prefix.`,
		Example: `  nexoctl share upload --expire 1h local/inbox/report.pdf
  nexoctl share upload --post --max-size 10MiB --prefix uploads/ local/inbox`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			managed, err := shareMode(cmd)
			if err != nil {
				return err
			}
			expiry, err := parseExpiry(expire)
			if err != nil {
				return err
			}
			t, err := e.remoteTarget(ctx, args[0], true)
			if err != nil {
				return err
			}
			if post {
				if cmd.Flags().Changed("managed") && managed {
					return cli.UsageError("POST policies are signed locally: --managed excludes --post")
				}
				client, err := e.client(ctx, t.Alias)
				if err != nil {
					return err
				}
				in := cli.PostPolicyInput{Bucket: t.Bucket, Expiry: expiry, ContentType: contentType}
				switch {
				case prefix != "":
					in.KeyPrefix = cli.DirPrefix(t.Key) + strings.TrimPrefix(prefix, "/")
					if t.Key != "" && !strings.HasSuffix(t.Key, "/") {
						return cli.UsageError("--prefix needs ALIAS/BUCKET or a target ending with \"/\"")
					}
				case t.Key == "" || strings.HasSuffix(t.Key, "/"):
					in.KeyPrefix = t.Key
				default:
					in.Key = t.Key
				}
				if maxSize != "" {
					if in.MaxSize, err = cli.ParseSize(maxSize); err != nil {
						return err
					}
				}
				if minSize != "" {
					if in.MinSize, err = cli.ParseSize(minSize); err != nil {
						return err
					}
					if in.MaxSize == 0 {
						return cli.UsageError("--min-size needs --max-size")
					}
				}
				form, err := client.PostPolicy(ctx, in)
				if err != nil {
					return fmt.Errorf("%s: %w", t, err)
				}
				return e.printPostPolicy(t, form, expiry)
			}
			if maxSize != "" || minSize != "" || prefix != "" {
				return cli.UsageError("--max-size, --min-size and --prefix need --post")
			}
			if t.Key == "" || strings.HasSuffix(t.Key, "/") {
				return cli.UsageError("%s: an upload URL needs an object name (or use --post with a prefix)", t)
			}
			var headers map[string]string
			if contentType != "" {
				headers = map[string]string{"Content-Type": contentType}
			}
			if managed {
				admin, err := e.admin(ctx, t.Alias)
				if err != nil {
					return err
				}
				sh, err := admin.CreateShare(ctx, nexoadmin.CreateShareRequest{
					Bucket: t.Bucket, Key: t.Key, Permission: nexoadmin.PermissionUpload, ExpiresIn: int64(expiry / time.Second), ContentType: contentType,
				})
				if err != nil {
					return fmt.Errorf("%s: %w", t, err)
				}
				command := cli.CurlCommand(&cli.PresignedRequest{Method: http.MethodPut, URL: sh.URL, Headers: headers})
				return e.printManagedShare(t, sh, http.MethodPut, headers, expiry, command)
			}
			client, err := e.client(ctx, t.Alias)
			if err != nil {
				return err
			}
			req, err := client.Presign(ctx, cli.PresignInput{Method: http.MethodPut, Bucket: t.Bucket, Key: t.Key, Expiry: expiry, Headers: headers})
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.printShare(t, req, "", expiry, cli.CurlCommand(req))
		},
	}
	f := cmd.Flags()
	f.StringVarP(&expire, "expire", "E", defaultShareExpiry, "validity of the link (1s to 7d)")
	f.StringVar(&contentType, "content-type", "", "Content-Type the upload must send")
	f.BoolVar(&post, "post", false, "generate a POST policy for HTML forms (signed locally)")
	f.StringVar(&maxSize, "max-size", "", "largest accepted upload with --post (e.g. 10MiB)")
	f.StringVar(&minSize, "min-size", "", "smallest accepted upload with --post")
	f.StringVar(&prefix, "prefix", "", "with --post: accept any key under this prefix of the bucket")
	shareModeFlags(cmd)
	return cmd
}

func newPresignCmd(e *env) *cobra.Command {
	var (
		method, expire, versionID, disposition, contentType string
		headers                                             []string
	)
	cmd := &cobra.Command{
		Use:   "presign [flags] ALIAS/BUCKET/OBJECT",
		Short: "Generate a raw presigned URL for any method",
		Long: `Generate a raw presigned URL (GET, HEAD, PUT or DELETE), signed locally with the
credentials of the alias: it is neither managed nor revocable.
Signed headers (--header) must be sent unchanged with the request.`,
		Example: `  nexoctl presign --method DELETE --expire 10m local/tmp/old.bin
  nexoctl presign --method PUT --header "Content-Type: image/png" local/img/logo.png`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			expiry, err := parseExpiry(expire)
			if err != nil {
				return err
			}
			hdrs := map[string]string{}
			for _, h := range headers {
				k, v, ok := strings.Cut(h, ":")
				if !ok || strings.TrimSpace(k) == "" {
					return cli.UsageError("invalid header %q (use \"Name: value\")", h)
				}
				hdrs[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			if contentType != "" {
				hdrs["Content-Type"] = contentType
			}
			m := strings.ToUpper(method)
			in := cli.PresignInput{Method: m, Expiry: expiry, VersionID: versionID}
			switch m {
			case http.MethodGet:
				in.ResponseContentDisposition = disposition
				if ct, ok := hdrs["Content-Type"]; ok {
					in.ResponseContentType = ct
					delete(hdrs, "Content-Type")
				}
			case http.MethodHead, http.MethodPut, http.MethodDelete:
				if disposition != "" {
					hdrs["Content-Disposition"] = disposition
				}
			default:
				return cli.UsageError("unsupported method %q (GET, HEAD, PUT, DELETE)", method)
			}
			if len(hdrs) > 0 {
				in.Headers = hdrs
			}
			t, err := e.remoteTarget(ctx, args[0], true)
			if err != nil {
				return err
			}
			if t.Key == "" || strings.HasSuffix(t.Key, "/") {
				return cli.UsageError("%s: a presigned URL needs an object name", t)
			}
			in.Bucket, in.Key = t.Bucket, t.Key
			client, err := e.client(ctx, t.Alias)
			if err != nil {
				return err
			}
			req, err := client.Presign(ctx, in)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			rec := shareRecord{
				Target: t.String(), Method: req.Method, URL: req.URL, Headers: req.Headers, VersionID: versionID,
				Expire: cli.HumanDuration(expiry), ExpiresAt: req.ExpiresAt, Command: cli.CurlCommand(req),
			}
			text := req.URL
			for _, k := range cli.SortedKeys(req.Headers) {
				text += fmt.Sprintf("\nHeader: %s: %s", k, req.Headers[k])
			}
			return e.out.Result(rec, text)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&method, "method", "X", http.MethodGet, "HTTP method: GET, HEAD, PUT or DELETE")
	f.StringVarP(&expire, "expire", "E", defaultShareExpiry, "validity of the URL (1s to 7d)")
	f.StringArrayVarP(&headers, "header", "H", nil, `signed header "Name: value" (PUT: Content-Type, Cache-Control, x-amz-meta-*, ...)`)
	f.StringVar(&versionID, "version-id", "", "version of the object (GET, HEAD, DELETE)")
	f.StringVar(&disposition, "content-disposition", "", "GET: Content-Disposition of the response; PUT: of the object")
	f.StringVar(&contentType, "content-type", "", "GET: Content-Type of the response; PUT: of the object")
	return cmd
}

// parseExpiry parses and checks --expire.
func parseExpiry(s string) (time.Duration, error) {
	d, err := cli.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if err := cli.CheckExpiry(d); err != nil {
		return 0, err
	}
	return d, nil
}

// printShare prints a presigned URL in the style of share.
func (e *env) printShare(t cli.Target, req *cli.PresignedRequest, versionID string, expiry time.Duration, command string) error {
	rec := shareRecord{
		Target: t.String(), Method: req.Method, URL: req.URL, Headers: req.Headers, VersionID: versionID,
		Expire: cli.HumanDuration(expiry), ExpiresAt: req.ExpiresAt, Command: command,
	}
	lines := []string{
		"URL: " + t.String(),
		"Expire: " + cli.HumanDuration(expiry),
		"Share: " + req.URL,
	}
	if command != "" {
		lines = append(lines, "Command: "+command)
	}
	return e.out.Result(rec, strings.Join(lines, "\n"))
}

// printPostPolicy prints a POST policy.
func (e *env) printPostPolicy(t cli.Target, form *cli.PostPolicyForm, expiry time.Duration) error {
	command := cli.CurlPostCommand(form)
	htmlForm := cli.HTMLPostForm(form)
	rec := postPolicyRecord{
		Target: t.String(), Method: http.MethodPost, URL: form.URL, Fields: form.Fields,
		Expire: cli.HumanDuration(expiry), ExpiresAt: form.ExpiresAt, Command: command, HTML: htmlForm,
	}
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\nExpire: %s\nPOST: %s\nFields:\n", t, cli.HumanDuration(expiry), form.URL)
	for _, k := range cli.SortedKeys(form.Fields) {
		fmt.Fprintf(&b, "  %s: %s\n", k, form.Fields[k])
	}
	fmt.Fprintf(&b, "Command: %s\nHTML:\n%s", command, htmlForm)
	return e.out.Result(rec, b.String())
}
