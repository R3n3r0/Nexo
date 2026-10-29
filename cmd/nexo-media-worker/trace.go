package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
	"github.com/r3n3r0/nexo/internal/mediaworker/transcode"
	"github.com/r3n3r0/nexo/internal/mediaworker/worker"
)

// newTraceCmd reads the forensic watermark of a leaked copy.
func newTraceCmd(deps cmdDeps) *cobra.Command {
	var (
		o                options
		leak, out        string
		bucket, key, vid string
		offset, every    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "trace --leak FILE --bucket BUCKET --key KEY",
		Short: "Read the forensic watermark of a leaked copy of a video",
		Long: `Read the forensic watermark of a leaked copy of a protected video: every
picture of the copy is compared with the two variants (A and B) of the
renditions the worker derived, at the same time of the video, and votes for
one of them. The votes (JSON) go to nexoctl media trace, which asks Nexo for
the playback token whose code they spell:

  nexo-media-worker trace --leak copy.mp4 --bucket lms --key courses/intro.mp4 --out votes.json
  nexoctl media trace local/lms/courses/intro.mp4 --votes votes.json

The copy must be an MP4 with H.264 video (convert another format first, for
example with ffmpeg -i copy.webm -c:v libx264 copy.mp4). A clip that does not
start at the beginning of the video needs --offset, the time of the video its
first picture shows. The worker reads the renditions with its S3 credentials
(the flags and NEXO_WORKER_* variables of the worker).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := applyEnv(cmd, deps.lookupEnv); err != nil {
				return err
			}
			if leak == "" || bucket == "" || key == "" {
				return errors.New("--leak, --bucket and --key are required")
			}
			if o.endpoint == "" || o.accessKey == "" || o.secretKey == "" {
				return errors.New("--s3-endpoint, --access-key and --secret-key are required (or NEXO_WORKER_S3_ENDPOINT, NEXO_WORKER_ACCESS_KEY, NEXO_WORKER_SECRET_KEY)")
			}
			ctx := cmd.Context()
			store, err := worker.NewS3Store(worker.S3Config{Endpoint: o.endpoint, Region: o.region, AccessKey: o.accessKey, SecretKey: o.secretKey})
			if err != nil {
				return err
			}
			libPath := o.openh264
			if libPath == "" {
				if libPath, err = openh264.Fetch(ctx, deps.client, o.downloadURL, filepath.Join(o.dataDir, "lib")); err != nil {
					return err
				}
			}
			lib, err := openh264.Open(libPath)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(o.dataDir, 0o750); err != nil {
				return err
			}
			res, err := worker.TraceLeak(ctx, store, lib, worker.TraceRequest{Bucket: bucket, Key: key, VersionID: vid, Leak: leak,
				WorkDir: o.dataDir, Options: transcode.TraceOptions{Offset: offset.Seconds(), Every: every.Seconds()}})
			if err != nil {
				return err
			}
			if out == "" || out == "-" {
				return writeVotes(deps.stdout, res)
			}
			f, err := os.Create(out) // #nosec G304 -- a path the operator names
			if err != nil {
				return err
			}
			if err := writeVotes(f, res); err != nil {
				_ = f.Close()
				return err
			}
			// The votes are the evidence of a trace: a failed close is a
			// failed write.
			if err := f.Close(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(deps.stderr, "%d votes of %s written to %s\n", len(res.Votes), leak, out)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&leak, "leak", "", "the leaked copy (an MP4 with H.264 video)")
	f.StringVar(&bucket, "bucket", "", "bucket of the video")
	f.StringVar(&key, "key", "", "key of the video")
	f.StringVar(&vid, "version-id", "", "version of the video (default the current one)")
	f.DurationVar(&offset, "offset", 0, "time of the video the first picture of the copy shows (a clip)")
	f.DurationVar(&every, "every", 200*time.Millisecond, "least time between two votes")
	f.StringVar(&out, "out", "", "write the votes to this file (default the standard output)")
	f.StringVar(&o.endpoint, "s3-endpoint", "", "URL of the S3 API of Nexo")
	f.StringVar(&o.region, "s3-region", "us-east-1", "region of the S3 API")
	f.StringVar(&o.accessKey, "access-key", "", "access key (prefer NEXO_WORKER_ACCESS_KEY)")
	f.StringVar(&o.secretKey, "secret-key", "", "secret key (prefer NEXO_WORKER_SECRET_KEY)")
	f.StringVar(&o.dataDir, "data-dir", "nexo-media-worker", "directory of the work files and the downloaded library")
	f.StringVar(&o.openh264, "openh264", "", "path of libopenh264 "+openh264.Version+" (default: the Cisco build, downloaded into the data directory)")
	f.StringVar(&o.downloadURL, "openh264-download-url", openh264.DownloadBase, "where the Cisco builds of OpenH264 are downloaded from")
	return cmd
}

// writeVotes writes the result of a trace as indented JSON.
func writeVotes(w io.Writer, res *worker.TraceResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
