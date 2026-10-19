// Command nexo-media-worker processes the media jobs of Nexo: it derives
// the renditions, the poster and the thumbnails of the videos selected by
// the media rules of the buckets, encoding H.264 with OpenH264 (the Cisco
// build, downloaded at the first start) and reading and writing the
// objects through the S3 API only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/r3n3r0/nexo/internal/mediaworker/imaging"
	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
	"github.com/r3n3r0/nexo/internal/mediaworker/worker"
	"github.com/r3n3r0/nexo/internal/version"
)

func main() {
	deps := cmdDeps{stdout: os.Stdout, stderr: os.Stderr, lookupEnv: os.LookupEnv, client: &http.Client{Timeout: 5 * time.Minute}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd(deps).ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// cmdDeps are the process-level dependencies of the commands.
type cmdDeps struct {
	stdout    io.Writer
	stderr    io.Writer
	lookupEnv func(string) (string, bool)
	client    *http.Client
}

// options are the settings of the worker; each flag has an environment
// variable (NEXO_WORKER_ + the flag name in upper case, '-' as '_').
type options struct {
	address     string
	authToken   string
	endpoint    string
	region      string
	accessKey   string
	secretKey   string
	dataDir     string
	concurrency int
	maxAttempts int
	name        string
	openh264    string
	downloadURL string
	logLevel    string
	logFormat   string
	imageBytes  int64
	imagePixels int64
	imageMemory int64
}

const defaultAddress = ":9600"

func newRootCmd(deps cmdDeps) *cobra.Command {
	var o options
	root := &cobra.Command{
		Use:   "nexo-media-worker",
		Short: "Nexo media worker: renditions, posters and thumbnails of videos, variants of images",
		Long: `nexo-media-worker receives the jobs of the media_worker notification target
of Nexo (POST /jobs, bearer auth_token), keeps them in a queue on disk and
derives the outputs the media rules of the buckets ask for: the renditions,
posters and thumbnails of videos, the variants of images (AVIF, WebP, JPEG,
PNG at several widths). It encodes H.264
with OpenH264: the Cisco build of the library is downloaded into the data
directory at the first start (Cisco licenses the H.264 patents of its build to
whoever downloads it), unless --openh264 names a library.

Every flag can be set with an environment variable: NEXO_WORKER_ followed by
the flag name in upper case (NEXO_WORKER_S3_ENDPOINT, NEXO_WORKER_AUTH_TOKEN...);
the credentials and the token can also be read from files named by
NEXO_WORKER_ACCESS_KEY_FILE, NEXO_WORKER_SECRET_KEY_FILE and
NEXO_WORKER_AUTH_TOKEN_FILE (Docker and Kubernetes secrets).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Get().String(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := applyEnv(cmd, deps.lookupEnv); err != nil {
				return err
			}
			return run(cmd.Context(), deps, o)
		},
	}
	root.SetVersionTemplate("nexo-media-worker {{.Version}}\n" + version.Get().LicenseLine() + "\n")
	root.SetOut(deps.stdout)
	root.SetErr(deps.stderr)
	f := root.Flags()
	f.StringVar(&o.address, "address", defaultAddress, "address of the jobs endpoint")
	f.StringVar(&o.authToken, "auth-token", "", "bearer token of the jobs endpoint: the auth_token of the media_worker target (required)")
	f.StringVar(&o.endpoint, "s3-endpoint", "", "URL of the S3 API of Nexo (required)")
	f.StringVar(&o.region, "s3-region", "us-east-1", "region of the S3 API")
	f.StringVar(&o.accessKey, "access-key", "", "access key of the worker (required; prefer NEXO_WORKER_ACCESS_KEY)")
	f.StringVar(&o.secretKey, "secret-key", "", "secret key of the worker (required; prefer NEXO_WORKER_SECRET_KEY)")
	f.StringVar(&o.dataDir, "data-dir", "nexo-media-worker", "directory of the job queue, the work files and the downloaded library")
	f.IntVar(&o.concurrency, "concurrency", worker.DefaultConcurrency, "jobs processed at once")
	f.IntVar(&o.maxAttempts, "max-attempts", worker.DefaultMaxAttempts, "attempts of a job that fails for a transient reason")
	f.StringVar(&o.name, "name", "", "name of the worker in status.json (default the host name)")
	f.StringVar(&o.openh264, "openh264", "", "path of libopenh264 "+openh264.Version+" (default: the Cisco build, downloaded into the data directory)")
	f.StringVar(&o.downloadURL, "openh264-download-url", openh264.DownloadBase, "where the Cisco builds of OpenH264 are downloaded from")
	f.StringVar(&o.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	f.StringVar(&o.logFormat, "log-format", "text", "log format: text or json")
	f.Int64Var(&o.imageBytes, "image-max-bytes", imaging.DefaultMaxBytes, "largest image file processed (a larger one fails for good)")
	f.Int64Var(&o.imagePixels, "image-max-pixels", imaging.DefaultMaxPixels, "largest image processed, in pixels, read from its header before decoding")
	f.Int64Var(&o.imageMemory, "image-memory", imaging.DefaultMemoryBudget, "memory of the image variants encoded at once, in bytes; a variant that alone needs more (an AVIF of a very large image) is not produced")
	root.AddCommand(newVersionCmd(deps), newHealthcheckCmd(deps), newFetchCmd(deps))
	return root
}

// applyEnv sets the flags not given on the command line from the
// environment.
func applyEnv(cmd *cobra.Command, lookupEnv func(string) (string, bool)) error {
	var err error
	cmd.Flags().VisitAll(func(fl *pflag.Flag) {
		if err != nil || fl.Changed {
			return
		}
		name := "NEXO_WORKER_" + strings.ToUpper(strings.ReplaceAll(fl.Name, "-", "_"))
		v, ok := lookupEnv(name)
		if !ok && secretFlags()[fl.Name] {
			// NAME_FILE: the value in a file (Docker and Kubernetes
			// secrets), its trailing newline removed.
			if path, fok := lookupEnv(name + "_FILE"); fok {
				data, rerr := os.ReadFile(path) // #nosec G304 -- a path the operator names
				if rerr != nil {
					err = fmt.Errorf("%s_FILE: %w", name, rerr)
					return
				}
				v, ok = strings.TrimRight(string(data), "\r\n"), true
			}
		}
		if ok {
			if serr := fl.Value.Set(v); serr != nil {
				err = fmt.Errorf("%s: %w", name, serr)
			}
		}
	})
	return err
}

// secretFlags are the flags that can also be read from a file named by
// NEXO_WORKER_<FLAG>_FILE.
func secretFlags() map[string]bool {
	return map[string]bool{"access-key": true, "secret-key": true, "auth-token": true}
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("--log-format: %q is neither text nor json", format)
}

func run(ctx context.Context, deps cmdDeps, o options) error {
	logger, err := newLogger(deps.stderr, o.logLevel, o.logFormat)
	if err != nil {
		return err
	}
	if o.authToken == "" {
		return errors.New("--auth-token (NEXO_WORKER_AUTH_TOKEN) is required: the auth_token of the media_worker target")
	}
	store, err := worker.NewS3Store(worker.S3Config{Endpoint: o.endpoint, Region: o.region, AccessKey: o.accessKey, SecretKey: o.secretKey})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.dataDir, 0o750); err != nil {
		return err
	}
	libPath := o.openh264
	if libPath == "" {
		logger.Info("checking the OpenH264 library", "version", openh264.Version, "dir", filepath.Join(o.dataDir, "lib"))
		libPath, err = openh264.Fetch(ctx, deps.client, o.downloadURL, filepath.Join(o.dataDir, "lib"))
		if err != nil {
			return err
		}
	}
	lib, err := openh264.Open(libPath)
	if err != nil {
		return err
	}
	logger.Info("OpenH264 loaded", "path", libPath, "version", lib.CodecVersion(),
		"license", "OpenH264 Video Codec provided by Cisco Systems, Inc.")
	w, err := worker.New(worker.Config{AuthToken: o.authToken, WorkDir: o.dataDir, Concurrency: o.concurrency, MaxAttempts: o.maxAttempts, Name: o.name,
		ImageLimits: imaging.Limits{MaxBytes: o.imageBytes, MaxPixels: o.imagePixels}, ImageMemory: o.imageMemory}, store, lib, logger)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.address)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: w.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("nexo-media-worker started", "version", version.Get().String(), "address", ln.Addr().String(), "s3", o.endpoint, "concurrency", o.concurrency)

	runCtx, cancel := context.WithCancel(ctx)
	running := make(chan struct{})
	go func() { w.Run(runCtx); close(running) }()
	select {
	case <-ctx.Done():
	case err = <-served:
	}
	logger.Info("nexo-media-worker stopping")
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	_ = srv.Shutdown(shutdownCtx)
	cancel()
	<-running // the running jobs stay queued for the next start
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func newVersionCmd(deps cmdDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()
			_, err := fmt.Fprintf(deps.stdout, "nexo-media-worker %s\n%s\n", info, info.LicenseLine())
			return err
		},
	}
}

// newFetchCmd downloads the library without starting the worker: to
// prepare a machine without Internet access, or a CI job.
func newFetchCmd(deps cmdDeps) *cobra.Command {
	var dir, downloadURL string
	cmd := &cobra.Command{
		Use:   "fetch-openh264",
		Short: "Download the Cisco build of OpenH264 for this platform and print its path",
		Long: `Download the Cisco build of OpenH264 ` + openh264.Version + ` for this platform into --dir (the lib
directory of the data directory by default, where the worker looks for it),
verify its pinned SHA-256 and print its path. Nothing is downloaded when a
verified copy is already there.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				dataDir := "nexo-media-worker"
				if v, ok := deps.lookupEnv("NEXO_WORKER_DATA_DIR"); ok && v != "" {
					dataDir = v
				}
				dir = filepath.Join(dataDir, "lib")
			}
			path, err := openh264.Fetch(cmd.Context(), deps.client, downloadURL, dir)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(deps.stdout, path)
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory of the library (default: the lib directory of NEXO_WORKER_DATA_DIR)")
	cmd.Flags().StringVar(&downloadURL, "openh264-download-url", openh264.DownloadBase, "where the Cisco builds of OpenH264 are downloaded from")
	return cmd
}

// newHealthcheckCmd checks a running worker (for the HEALTHCHECK of an
// image without a shell).
func newHealthcheckCmd(deps cmdDeps) *cobra.Command {
	var address string
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Check that the worker answers on its address",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if v, ok := deps.lookupEnv("NEXO_WORKER_ADDRESS"); ok && !cmd.Flags().Changed("address") {
				address = v
			}
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("--address: %w", err)
			}
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			if _, err := strconv.Atoi(port); err != nil {
				return fmt.Errorf("--address: invalid port %q", port)
			}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
			if err != nil {
				return err
			}
			client := *deps.client
			client.Timeout = 5 * time.Second
			resp, err := client.Do(req) // #nosec G704 -- the local worker
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			var health struct {
				Status  string `json:"status"`
				Pending int    `json:"pending"`
			}
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&health) != nil || health.Status != "ok" {
				return fmt.Errorf("the worker is not healthy: %s", resp.Status)
			}
			_, err = fmt.Fprintf(deps.stdout, "ok (%d pending)\n", health.Pending)
			return err
		},
	}
	cmd.Flags().StringVar(&address, "address", defaultAddress, "address of the worker")
	return cmd
}
