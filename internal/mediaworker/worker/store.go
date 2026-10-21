// Package worker runs the jobs of the media worker: it receives them from
// the media_worker target of Nexo, keeps them in a queue on disk until
// they are done, and processes them through the S3 API only (no internal
// RPC, no admin rights): it reads the video, writes the renditions, the
// poster and the thumbnails into the sidecar directory of its version, and
// the status.json Nexo reads to serve them.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// ErrNotFound reports a missing object or version.
var ErrNotFound = errors.New("not found")

// ErrPrecondition reports a conditional write refused: the object changed
// since it was read.
var ErrPrecondition = errors.New("precondition failed")

// ObjectInfo describes an object version.
type ObjectInfo struct {
	VersionID string
	ETag      string
	Size      int64
}

// ObjectVersion names a version of an object of a listing ("null" in an
// unversioned bucket).
type ObjectVersion struct {
	Key       string
	VersionID string
	// Latest is the current version of its key (a delete marker never is).
	Latest bool
}

// Store is the S3 API the worker uses.
type Store interface {
	// Head returns the version of an object ("" the current one).
	Head(ctx context.Context, bucket, key, versionID string) (ObjectInfo, error)
	// Download writes a version of an object to path.
	Download(ctx context.Context, bucket, key, versionID, path string) error
	// Upload stores the file at path as an object.
	Upload(ctx context.Context, bucket, key, path, contentType string) error
	// Put stores data as an object.
	Put(ctx context.Context, bucket, key string, data []byte, contentType string) error
	// Get reads a small object (at most limit bytes) and its ETag.
	Get(ctx context.Context, bucket, key string, limit int64) ([]byte, string, error)
	// PutIf stores data as an object if its current ETag is ifMatch, or,
	// ifMatch empty, if there is no object: ErrPrecondition otherwise.
	PutIf(ctx context.Context, bucket, key string, data []byte, contentType, ifMatch string) error
	// List returns every version (delete markers included) of the keys
	// under a prefix.
	List(ctx context.Context, bucket, prefix string) ([]ObjectVersion, error)
	// Delete removes versions for good: the worker only deletes what it
	// derived, under the sidecar prefix, so that a versioned bucket keeps
	// no stale outputs.
	Delete(ctx context.Context, bucket string, versions []ObjectVersion) error
}

// S3Config configures the S3 client of the worker.
type S3Config struct {
	// Endpoint is the URL of the S3 API of Nexo.
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	// HTTP performs the requests (default http.DefaultClient).
	HTTP *http.Client
}

type s3Store struct {
	api *s3.Client
}

// NewS3Store returns the Store of an S3 endpoint (path-style requests).
func NewS3Store(cfg S3Config) (Store, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("worker: the S3 endpoint %q must be an http(s) URL", cfg.Endpoint)
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("worker: the S3 access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	client := cfg.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		HTTPClient:  client,
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 3 })
		},
	}
	api := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(u.String())
		o.UsePathStyle = true
	})
	return &s3Store{api: api}, nil
}

func optVersion(v string) *string {
	if v == "" || v == "null" {
		return nil
	}
	return aws.String(v)
}

// notFound maps the missing objects and versions to ErrNotFound.
func notFound(err error) error {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

func (s *s3Store) Head(ctx context.Context, bucket, key, versionID string) (ObjectInfo, error) {
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: optVersion(versionID)})
	if err != nil {
		return ObjectInfo{}, notFound(err)
	}
	return ObjectInfo{VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag), Size: aws.ToInt64(out.ContentLength)}, nil
}

func (s *s3Store) Download(ctx context.Context, bucket, key, versionID, path string) error {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: optVersion(versionID)})
	if err != nil {
		return notFound(err)
	}
	defer func() { _ = out.Body.Close() }()
	f, err := os.Create(path) // #nosec G304 -- a file of the job directory
	if err != nil {
		return err
	}
	_, err = io.Copy(f, out.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *s3Store) Upload(ctx context.Context, bucket, key, path, contentType string) error {
	f, err := os.Open(path) // #nosec G304 -- a file of the job directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: f,
		ContentLength: aws.Int64(st.Size()), ContentType: aws.String(contentType)})
	return err
}

func (s *s3Store) Put(ctx context.Context, bucket, key string, data []byte, contentType string) error {
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader(string(data)), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(contentType)})
	return err
}

func (s *s3Store) Get(ctx context.Context, bucket, key string, limit int64) ([]byte, string, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, "", notFound(err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("worker: %s/%s is larger than %d bytes", bucket, key, limit)
	}
	return data, aws.ToString(out.ETag), nil
}

func (s *s3Store) PutIf(ctx context.Context, bucket, key string, data []byte, contentType, ifMatch string) error {
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader(string(data)), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(contentType)}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	} else {
		in.IfNoneMatch = aws.String("*")
	}
	_, err := s.api.PutObject(ctx, in)
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && (re.HTTPStatusCode() == http.StatusPreconditionFailed || re.HTTPStatusCode() == http.StatusConflict) {
		return fmt.Errorf("%w: %w", ErrPrecondition, err)
	}
	return err
}

func (s *s3Store) List(ctx context.Context, bucket, prefix string) ([]ObjectVersion, error) {
	var out []ObjectVersion
	p := s3.NewListObjectVersionsPaginator(s.api, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, notFound(err)
		}
		for _, v := range page.Versions {
			out = append(out, ObjectVersion{Key: aws.ToString(v.Key), VersionID: aws.ToString(v.VersionId), Latest: aws.ToBool(v.IsLatest)})
		}
		for _, m := range page.DeleteMarkers {
			out = append(out, ObjectVersion{Key: aws.ToString(m.Key), VersionID: aws.ToString(m.VersionId)})
		}
	}
	return out, nil
}

func (s *s3Store) Delete(ctx context.Context, bucket string, versions []ObjectVersion) error {
	for len(versions) > 0 {
		n := min(len(versions), 1000)
		ids := make([]types.ObjectIdentifier, 0, n)
		for _, v := range versions[:n] {
			id := types.ObjectIdentifier{Key: aws.String(v.Key)}
			if v.VersionID != "" {
				// "null" too: without a version, a versioned bucket would
				// only add a delete marker.
				id.VersionId = aws.String(v.VersionID)
			}
			ids = append(ids, id)
		}
		out, err := s.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("worker: delete %s: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
		versions = versions[n:]
	}
	return nil
}
