package tier

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Content-MD5 is the integrity check of the S3 API
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/r3n3r0/nexo/internal/domain"
)

const (
	// minPartSize is the part size of the multipart writes; an object up
	// to it is written in one request.
	minPartSize = 16 << 20
	// maxParts is the S3 limit of parts of a multipart upload.
	maxParts = 10000
	// listPage is the objects listed per request.
	listPage = 1000
)

// s3Dialer opens S3 clients of the tiers.
type s3Dialer struct {
	client *http.Client
}

var _ Dialer = s3Dialer{}

// NewS3Dialer returns the Dialer of the S3 API (path-style requests,
// SigV4). client nil uses a client with connection and response timeouts
// and no overall one (a large object takes its time).
func NewS3Dialer(client *http.Client) Dialer {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 2 * time.Minute
		tr.MaxIdleConnsPerHost = 16
		client = &http.Client{Transport: tr}
	}
	return s3Dialer{client: client}
}

// Dial implements Dialer.
func (d s3Dialer) Dial(_ context.Context, cfg RemoteConfig) (Remote, error) {
	if err := validEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	c := s3.New(s3.Options{
		Region:                     region,
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		BaseEndpoint:               aws.String(cfg.Endpoint),
		UsePathStyle:               true,
		HTTPClient:                 d.client,
		RetryMaxAttempts:           3,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &s3Remote{client: c, bucket: cfg.Bucket, class: cfg.StorageClass}, nil
}

// s3Remote is the bucket of a tier reached through the S3 API.
type s3Remote struct {
	client *s3.Client
	bucket string
	class  string
}

var _ Remote = (*s3Remote)(nil)

func notFound(err error) bool {
	var re *smithyhttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound
}

func contentMD5(b []byte) *string {
	sum := md5.Sum(b) // #nosec G401 -- Content-MD5
	return aws.String(base64.StdEncoding.EncodeToString(sum[:]))
}

// CheckBucket implements Remote.
func (r *s3Remote) CheckBucket(ctx context.Context) error {
	_, err := r.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(r.bucket), MaxKeys: aws.Int32(1)})
	return err
}

// partSize is the part size of an object of size bytes.
func partSize(size int64) int64 {
	p := int64(minPartSize)
	if need := (size + maxParts - 1) / maxParts; need > p {
		p = need
	}
	return p
}

// Put implements Remote.
func (r *s3Remote) Put(ctx context.Context, name string, body io.Reader, size int64, meta map[string]string) error {
	if size < 0 {
		return fmt.Errorf("%w: tier: unknown size", domain.ErrInvalidArgument)
	}
	if size <= minPartSize {
		buf := make([]byte, size)
		if _, err := io.ReadFull(body, buf); err != nil {
			return fmt.Errorf("read the data: %w", err)
		}
		if err := noMore(body); err != nil {
			return err
		}
		in := &s3.PutObjectInput{
			Bucket: aws.String(r.bucket), Key: aws.String(name), Body: bytes.NewReader(buf),
			ContentLength: aws.Int64(size), ContentMD5: contentMD5(buf), Metadata: meta,
			ContentType: aws.String("application/octet-stream"),
		}
		if r.class != "" {
			in.StorageClass = types.StorageClass(r.class)
		}
		if _, err := r.client.PutObject(ctx, in); err != nil {
			return err
		}
	} else if err := r.putMultipart(ctx, name, body, size, meta); err != nil {
		return err
	}
	o, err := r.Head(ctx, name)
	if err != nil {
		return fmt.Errorf("check the written object: %w", err)
	}
	if o.Size != size {
		_ = r.Delete(context.WithoutCancel(ctx), name)
		return fmt.Errorf("%w: the written object has %d bytes, %d expected", domain.ErrFaultyDisk, o.Size, size)
	}
	return nil
}

// noMore fails when r has bytes beyond the declared size.
func noMore(r io.Reader) error {
	var one [1]byte
	if n, _ := r.Read(one[:]); n > 0 {
		return fmt.Errorf("%w: tier: more data than declared", domain.ErrInvalidArgument)
	}
	return nil
}

// putMultipart writes an object in parts, aborting the upload on failure.
func (r *s3Remote) putMultipart(ctx context.Context, name string, body io.Reader, size int64, meta map[string]string) error {
	in := &s3.CreateMultipartUploadInput{Bucket: aws.String(r.bucket), Key: aws.String(name), Metadata: meta, ContentType: aws.String("application/octet-stream")}
	if r.class != "" {
		in.StorageClass = types.StorageClass(r.class)
	}
	up, err := r.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return err
	}
	uploadID := up.UploadId
	abort := func() {
		_, _ = r.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{Bucket: aws.String(r.bucket), Key: aws.String(name), UploadId: uploadID})
	}
	ps := partSize(size)
	buf := make([]byte, ps)
	done := &types.CompletedMultipartUpload{}
	for n, left := int32(1), size; left > 0; n++ {
		chunk := buf[:min(ps, left)]
		if _, err := io.ReadFull(body, chunk); err != nil {
			abort()
			return fmt.Errorf("read the data: %w", err)
		}
		out, err := r.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(r.bucket), Key: aws.String(name), UploadId: uploadID, PartNumber: aws.Int32(n),
			Body: bytes.NewReader(chunk), ContentLength: aws.Int64(int64(len(chunk))), ContentMD5: contentMD5(chunk),
		})
		if err != nil {
			abort()
			return err
		}
		done.Parts = append(done.Parts, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag})
		left -= int64(len(chunk))
	}
	if err := noMore(body); err != nil {
		abort()
		return err
	}
	if _, err := r.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(r.bucket), Key: aws.String(name), UploadId: uploadID, MultipartUpload: done,
	}); err != nil {
		abort()
		return err
	}
	return nil
}

// Head implements Remote.
func (r *s3Remote) Head(ctx context.Context, name string) (RemoteObject, error) {
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(name)})
	if err != nil {
		if notFound(err) {
			return RemoteObject{}, fmt.Errorf("%w: %s", domain.ErrObjectNotFound, name)
		}
		return RemoteObject{}, err
	}
	return RemoteObject{Name: name, Size: aws.ToInt64(out.ContentLength), ModTime: aws.ToTime(out.LastModified), Metadata: out.Metadata}, nil
}

// Open implements Remote.
func (r *s3Remote) Open(ctx context.Context, name string, offset, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket), Key: aws.String(name),
		Range: aws.String("bytes=" + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+length-1, 10)),
	})
	if err != nil {
		if notFound(err) {
			return nil, fmt.Errorf("%w: the object %s is missing from the tier", domain.ErrFaultyDisk, name)
		}
		return nil, err
	}
	if got := aws.ToInt64(out.ContentLength); got != length {
		_ = out.Body.Close()
		return nil, fmt.Errorf("%w: the tier served %d bytes of %s, %d asked", domain.ErrFaultyDisk, got, name, length)
	}
	return &exactReader{rc: out.Body, left: length}, nil
}

// exactReader fails a stream that ends before its announced length.
type exactReader struct {
	rc   io.ReadCloser
	left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.rc.Read(p)
	e.left -= int64(n)
	if errors.Is(err, io.EOF) {
		if e.left > 0 {
			return n, io.ErrUnexpectedEOF
		}
		return n, io.EOF
	}
	return n, err
}

func (e *exactReader) Close() error { return e.rc.Close() }

// Delete implements Remote.
func (r *s3Remote) Delete(ctx context.Context, name string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(name)})
	if err != nil && !notFound(err) {
		return err
	}
	return nil
}

// List implements Remote.
func (r *s3Remote) List(ctx context.Context, prefix, token string) ([]RemoteObject, string, error) {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(r.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(listPage)}
	if token != "" {
		in.ContinuationToken = aws.String(token)
	}
	out, err := r.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, "", err
	}
	objs := make([]RemoteObject, 0, len(out.Contents))
	for _, o := range out.Contents {
		objs = append(objs, RemoteObject{Name: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), ModTime: aws.ToTime(o.LastModified)})
	}
	next := ""
	if aws.ToBool(out.IsTruncated) {
		next = aws.ToString(out.NextContinuationToken)
	}
	return objs, next, nil
}
