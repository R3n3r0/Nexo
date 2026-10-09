package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// abortTimeout bounds the AbortMultipartUpload sent after a failed or
// interrupted upload: it runs on a context detached from the canceled one.
const abortTimeout = 30 * time.Second

// deleteBatch is the maximum number of keys of one DeleteObjects request.
const deleteBatch = 1000

// S3FactoryDeps are the dependencies of the aws-sdk-go-v2 S3Factory.
type S3FactoryDeps struct {
	// Clock dates the presigned URLs and the POST policies (nil: the wall
	// clock).
	Clock clock.Clock
	// Transport is the base HTTP transport of the clients (nil: a clone of
	// http.DefaultTransport).
	Transport http.RoundTripper
	// Logger receives the diagnostics of the clients (nil: discarded).
	Logger *slog.Logger
}

type s3Factory struct {
	deps S3FactoryDeps
}

var _ S3Factory = (*s3Factory)(nil)

// NewS3Factory returns the S3Factory of nexoctl, built on aws-sdk-go-v2:
// the endpoint, the credentials, the region and the URL style come from
// the alias.
func NewS3Factory(deps S3FactoryDeps) S3Factory {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Transport == nil {
		deps.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	return &s3Factory{deps: deps}
}

// Client implements S3Factory.
func (f *s3Factory) Client(_ context.Context, a Alias, opts ClientOptions) (S3Client, error) {
	if err := ValidateAlias(a); err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(strings.TrimRight(a.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("%w: alias URL %q: %v", domain.ErrInvalidArgument, a.URL, err)
	}
	region := a.Region
	if region == "" {
		region = DefaultRegion
	}
	rt := f.deps.Transport
	if opts.LimitUpload > 0 || opts.LimitDownload > 0 {
		rt = newLimitTransport(rt, opts.LimitUpload, opts.LimitDownload)
	}
	if opts.Debug != nil {
		rt = newDebugTransport(rt, opts.Debug)
	}
	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(a.AccessKey, a.SecretKey, ""),
		// No overall timeout: transfers of large objects take as long as
		// they take; Ctrl-C cancels the context.
		HTTPClient:       &http.Client{Transport: rt},
		RetryMaxAttempts: 3,
	}
	pathStyle := UsePathStyle(a)
	api := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint.String())
		o.UsePathStyle = pathStyle
	})
	return &sdkClient{
		api:       api,
		presign:   s3.NewPresignClient(api),
		alias:     a,
		region:    region,
		endpoint:  endpoint,
		pathStyle: pathStyle,
		clock:     f.deps.Clock,
		logger:    f.deps.Logger.With("alias", a.Name),
	}, nil
}

// UsePathStyle reports whether the requests of an alias address the bucket
// in the path ("on", and "auto" except for AWS endpoints) rather than in
// the host name ("off").
func UsePathStyle(a Alias) bool {
	switch strings.ToLower(a.Path) {
	case "on":
		return true
	case "off":
		return false
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return true
	}
	return !strings.HasSuffix(strings.ToLower(u.Hostname()), ".amazonaws.com")
}

// sdkClient is the S3Client of one alias.
type sdkClient struct {
	api       *s3.Client
	presign   *s3.PresignClient
	alias     Alias
	region    string
	endpoint  *url.URL
	pathStyle bool
	clock     clock.Clock
	logger    *slog.Logger
}

var _ S3Client = (*sdkClient)(nil)

// ListBuckets implements S3Client.
func (c *sdkClient) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	in := &s3.ListBucketsInput{}
	var out []BucketInfo
	for {
		page, err := c.api.ListBuckets(ctx, in)
		if err != nil {
			return nil, mapS3Error(err, "", "")
		}
		for _, b := range page.Buckets {
			out = append(out, BucketInfo{Name: aws.ToString(b.Name), Created: aws.ToTime(b.CreationDate).UTC()})
		}
		token := aws.ToString(page.ContinuationToken)
		if token == "" || token == aws.ToString(in.ContinuationToken) {
			break
		}
		in.ContinuationToken = aws.String(token)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// BucketExists implements S3Client.
func (c *sdkClient) BucketExists(ctx context.Context, bucket string) (bool, error) {
	_, err := c.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return true, nil
	}
	err = mapS3Error(err, bucket, "")
	if errors.Is(err, domain.ErrBucketNotFound) {
		return false, nil
	}
	return false, err
}

// MakeBucket implements S3Client.
func (c *sdkClient) MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error {
	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	region := opts.Region
	if region == "" {
		region = c.region
	}
	if region != "" && region != DefaultRegion {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	if opts.ObjectLock {
		in.ObjectLockEnabledForBucket = aws.Bool(true)
	}
	_, err := c.api.CreateBucket(ctx, in)
	return mapS3Error(err, bucket, "")
}

// RemoveBucket implements S3Client.
func (c *sdkClient) RemoveBucket(ctx context.Context, bucket string) error {
	_, err := c.api.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	return mapS3Error(err, bucket, "")
}

// List implements S3Client.
func (c *sdkClient) List(ctx context.Context, bucket string, opts ListOptions) iter.Seq2[ObjectInfo, error] {
	if opts.Versions {
		return c.listVersions(ctx, bucket, opts)
	}
	return func(yield func(ObjectInfo, error) bool) {
		in := &s3.ListObjectsV2Input{Bucket: aws.String(bucket)}
		if opts.Prefix != "" {
			in.Prefix = aws.String(opts.Prefix)
		}
		if !opts.Recursive {
			in.Delimiter = aws.String("/")
		}
		pages := s3.NewListObjectsV2Paginator(c.api, in)
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				yield(ObjectInfo{}, mapS3Error(err, bucket, ""))
				return
			}
			entries := make([]ObjectInfo, 0, len(page.Contents)+len(page.CommonPrefixes))
			for _, o := range page.Contents {
				entries = append(entries, ObjectInfo{
					Key:          aws.ToString(o.Key),
					Size:         aws.ToInt64(o.Size),
					ETag:         trimETag(aws.ToString(o.ETag)),
					LastModified: aws.ToTime(o.LastModified).UTC(),
					StorageClass: string(o.StorageClass),
					IsLatest:     true,
				})
			}
			for _, p := range page.CommonPrefixes {
				entries = append(entries, ObjectInfo{Key: aws.ToString(p.Prefix), Prefix: true})
			}
			sort.SliceStable(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
			for _, e := range entries {
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}

// listVersions iterates over ListObjectVersions: for each key the latest
// version first.
func (c *sdkClient) listVersions(ctx context.Context, bucket string, opts ListOptions) iter.Seq2[ObjectInfo, error] {
	return func(yield func(ObjectInfo, error) bool) {
		in := &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)}
		if opts.Prefix != "" {
			in.Prefix = aws.String(opts.Prefix)
		}
		if !opts.Recursive {
			in.Delimiter = aws.String("/")
		}
		for {
			page, err := c.api.ListObjectVersions(ctx, in)
			if err != nil {
				yield(ObjectInfo{}, mapS3Error(err, bucket, ""))
				return
			}
			entries := make([]ObjectInfo, 0, len(page.Versions)+len(page.DeleteMarkers)+len(page.CommonPrefixes))
			for _, v := range page.Versions {
				entries = append(entries, ObjectInfo{
					Key:          aws.ToString(v.Key),
					Size:         aws.ToInt64(v.Size),
					ETag:         trimETag(aws.ToString(v.ETag)),
					LastModified: aws.ToTime(v.LastModified).UTC(),
					StorageClass: string(v.StorageClass),
					VersionID:    aws.ToString(v.VersionId),
					IsLatest:     aws.ToBool(v.IsLatest),
				})
			}
			for _, m := range page.DeleteMarkers {
				entries = append(entries, ObjectInfo{
					Key:          aws.ToString(m.Key),
					LastModified: aws.ToTime(m.LastModified).UTC(),
					VersionID:    aws.ToString(m.VersionId),
					IsLatest:     aws.ToBool(m.IsLatest),
					DeleteMarker: true,
				})
			}
			for _, p := range page.CommonPrefixes {
				entries = append(entries, ObjectInfo{Key: aws.ToString(p.Prefix), Prefix: true})
			}
			SortVersions(entries)
			for _, e := range entries {
				if !yield(e, nil) {
					return
				}
			}
			if !aws.ToBool(page.IsTruncated) {
				return
			}
			nextKey, nextVersion := aws.ToString(page.NextKeyMarker), aws.ToString(page.NextVersionIdMarker)
			if nextKey == "" || (nextKey == aws.ToString(in.KeyMarker) && nextVersion == aws.ToString(in.VersionIdMarker)) {
				return
			}
			in.KeyMarker, in.VersionIdMarker = aws.String(nextKey), aws.String(nextVersion)
		}
	}
}

// SortVersions orders a listing of versions by key, and for each key the
// latest version first, then from the newest to the oldest.
func SortVersions(entries []ObjectInfo) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.IsLatest != b.IsLatest {
			return a.IsLatest
		}
		return a.LastModified.After(b.LastModified)
	})
}

// Stat implements S3Client.
func (c *sdkClient) Stat(ctx context.Context, bucket, key string, opts StatOptions) (*ObjectInfo, error) {
	in := &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if opts.VersionID != "" {
		in.VersionId = aws.String(opts.VersionID)
	}
	if opts.Checksum {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	ck := customerKeyOf(opts.SSE)
	in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = ck.algorithm, ck.key, ck.keyMD5
	out, err := c.api.HeadObject(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, bucket, key)
	}
	return &ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         trimETag(aws.ToString(out.ETag)),
		LastModified: aws.ToTime(out.LastModified).UTC(),
		ContentType:  aws.ToString(out.ContentType),
		VersionID:    aws.ToString(out.VersionId),
		IsLatest:     opts.VersionID == "",
		DeleteMarker: aws.ToBool(out.DeleteMarker),
		Metadata:     lowerKeys(out.Metadata),
		StorageClass: string(out.StorageClass),
		Retention:    retentionOf(out.ObjectLockMode, out.ObjectLockRetainUntilDate),
		LegalHold:    domain.LegalHoldStatus(out.ObjectLockLegalHoldStatus),
		Checksum: checksumOf(checksumValues{out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME, out.ChecksumSHA1, out.ChecksumSHA256},
			out.ChecksumType),
		Encryption: encryptionOf(out.ServerSideEncryption, out.SSEKMSKeyId, out.SSECustomerAlgorithm, out.SSECustomerKeyMD5),
	}, nil
}

// Get implements S3Client.
func (c *sdkClient) Get(ctx context.Context, bucket, key string, opts GetOptions) (io.ReadCloser, *ObjectInfo, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if opts.VersionID != "" {
		in.VersionId = aws.String(opts.VersionID)
	}
	ck := customerKeyOf(opts.SSE)
	in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = ck.algorithm, ck.key, ck.keyMD5
	if r := opts.Range; r != nil {
		if r.Offset < 0 || r.Length < 0 {
			return nil, nil, fmt.Errorf("%w: invalid range %d+%d", domain.ErrInvalidArgument, r.Offset, r.Length)
		}
		if r.Length == 0 {
			in.Range = aws.String(fmt.Sprintf("bytes=%d-", r.Offset))
		} else {
			in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", r.Offset, r.Offset+r.Length-1))
		}
	}
	out, err := c.api.GetObject(ctx, in)
	if err != nil {
		return nil, nil, mapS3Error(err, bucket, key)
	}
	info := &ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         trimETag(aws.ToString(out.ETag)),
		LastModified: aws.ToTime(out.LastModified).UTC(),
		ContentType:  aws.ToString(out.ContentType),
		VersionID:    aws.ToString(out.VersionId),
		IsLatest:     opts.VersionID == "",
		Metadata:     lowerKeys(out.Metadata),
		StorageClass: string(out.StorageClass),
		Retention:    retentionOf(out.ObjectLockMode, out.ObjectLockRetainUntilDate),
		LegalHold:    domain.LegalHoldStatus(out.ObjectLockLegalHoldStatus),
		Encryption:   encryptionOf(out.ServerSideEncryption, out.SSEKMSKeyId, out.SSECustomerAlgorithm, out.SSECustomerKeyMD5),
	}
	return out.Body, info, nil
}

// Put implements S3Client: a single PutObject when the object fits in one
// part, otherwise a multipart upload whose parts are uploaded by
// opts.Parallel workers.
func (c *sdkClient) Put(ctx context.Context, bucket, key string, body io.Reader, opts PutOptions) (*PutResult, error) {
	if err := CheckPartSize(opts.PartSize); err != nil {
		return nil, err
	}
	if opts.Size < 0 {
		return c.putStream(ctx, bucket, key, body, opts)
	}
	partSize, parts, err := PartSize(opts.Size, opts.PartSize)
	if err != nil {
		return nil, err
	}
	if parts <= 1 && opts.Size <= partSize {
		data, err := sizedBody(body, opts.Size)
		if err != nil {
			return nil, err
		}
		return c.putSingle(ctx, bucket, key, data, opts.Size, opts)
	}
	var src partSource
	if ra, ok := readerAt(body); ok {
		src = &sectionParts{ra: ra, size: opts.Size, partSize: partSize, parts: parts}
	} else {
		src = &streamParts{r: body, size: opts.Size, partSize: partSize}
	}
	return c.putMultipart(ctx, bucket, key, src, opts)
}

// putStream uploads a body of unknown length: a single PutObject when it
// ends within the first part, a multipart upload otherwise.
func (c *sdkClient) putStream(ctx context.Context, bucket, key string, body io.Reader, opts PutOptions) (*PutResult, error) {
	first := make([]byte, StreamPartSize(1, opts.PartSize))
	n, err := io.ReadFull(body, first)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return c.putSingle(ctx, bucket, key, bytes.NewReader(first[:n]), int64(n), opts)
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	src := &streamParts{r: body, size: -1, partSize: opts.PartSize, first: first}
	return c.putMultipart(ctx, bucket, key, src, opts)
}

// putSingle uploads a whole object with PutObject.
func (c *sdkClient) putSingle(ctx context.Context, bucket, key string, body io.ReadSeeker, size int64, opts PutOptions) (*PutResult, error) {
	in := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		Metadata:      opts.Metadata,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if len(opts.Tags) > 0 {
		in.Tagging = aws.String(FormatTags(opts.Tags))
	}
	sse := sseOf(opts.SSE)
	in.ServerSideEncryption, in.SSEKMSKeyId = sse.algorithm, sse.kmsKeyID
	in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = sse.customer.algorithm, sse.customer.key, sse.customer.keyMD5
	if opts.Checksum != domain.ChecksumNone {
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(opts.Checksum)
	}
	out, err := c.api.PutObject(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, bucket, key)
	}
	if opts.Progress != nil {
		opts.Progress(size)
	}
	res := &PutResult{ETag: trimETag(aws.ToString(out.ETag)), VersionID: aws.ToString(out.VersionId), Size: size}
	if opts.Checksum != domain.ChecksumNone {
		res.Checksum = checksumOf(checksumValues{out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME, out.ChecksumSHA1, out.ChecksumSHA256}, out.ChecksumType)
	}
	return res, nil
}

// putMultipart runs a multipart upload and aborts it on failure.
func (c *sdkClient) putMultipart(ctx context.Context, bucket, key string, src partSource, opts PutOptions) (*PutResult, error) {
	in := &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), Metadata: opts.Metadata}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if len(opts.Tags) > 0 {
		in.Tagging = aws.String(FormatTags(opts.Tags))
	}
	sse := sseOf(opts.SSE)
	in.ServerSideEncryption, in.SSEKMSKeyId = sse.algorithm, sse.kmsKeyID
	in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = sse.customer.algorithm, sse.customer.key, sse.customer.keyMD5
	if opts.Checksum != domain.ChecksumNone {
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(opts.Checksum)
	}
	created, err := c.createUpload(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, bucket, key)
	}
	uploadID := aws.ToString(created.UploadId)
	upload := func(ctx context.Context, p part) (types.CompletedPart, error) {
		pin := &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
			PartNumber: aws.Int32(p.number), Body: p.body, ContentLength: aws.Int64(p.size),
			SSECustomerAlgorithm: sse.customer.algorithm, SSECustomerKey: sse.customer.key, SSECustomerKeyMD5: sse.customer.keyMD5,
		}
		if opts.Checksum != domain.ChecksumNone {
			pin.ChecksumAlgorithm = types.ChecksumAlgorithm(opts.Checksum)
		}
		out, err := c.api.UploadPart(ctx, pin)
		if err != nil {
			return types.CompletedPart{}, mapS3Error(err, bucket, key)
		}
		done := types.CompletedPart{PartNumber: aws.Int32(p.number), ETag: out.ETag}
		if opts.Checksum != domain.ChecksumNone {
			setPartChecksum(&done, checksumValues{out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME, out.ChecksumSHA1, out.ChecksumSHA256})
		}
		return done, nil
	}
	parts, size, err := runParts(ctx, src, opts.Parallel, opts.Progress, upload)
	if err == nil {
		var done *s3.CompleteMultipartUploadOutput
		done, err = c.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		})
		if err == nil {
			res := &PutResult{ETag: trimETag(aws.ToString(done.ETag)), VersionID: aws.ToString(done.VersionId), Size: size, Parts: len(parts)}
			if opts.Checksum != domain.ChecksumNone {
				res.Checksum = checksumOf(checksumValues{done.ChecksumCRC32, done.ChecksumCRC32C, done.ChecksumCRC64NVME, done.ChecksumSHA1, done.ChecksumSHA256}, done.ChecksumType)
			}
			return res, nil
		}
		err = mapS3Error(err, bucket, key)
	}
	return nil, c.abort(ctx, bucket, key, uploadID, err)
}

// createUpload starts a multipart upload on a context detached from the
// cancellation of ctx: an interruption during the request would leave an
// upload on the server whose ID the client never learns. The caller checks
// ctx afterwards and aborts the upload.
func (c *sdkClient) createUpload(ctx context.Context, in *s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	return c.api.CreateMultipartUpload(detached, in)
}

// abort aborts a failed multipart upload on a context detached from ctx
// (which may be canceled) and returns the *AbortedUploadError.
func (c *sdkClient) abort(ctx context.Context, bucket, key, uploadID string, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = ctxErr
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	_, abortErr := c.api.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if abortErr != nil {
		abortErr = mapS3Error(abortErr, bucket, key)
		c.logger.Warn("abort multipart upload", "bucket", bucket, "key", key, "upload", uploadID, "err", abortErr)
	}
	return &AbortedUploadError{Bucket: bucket, Key: key, UploadID: uploadID, Err: cause, AbortErr: abortErr}
}

// Copy implements S3Client.
func (c *sdkClient) Copy(ctx context.Context, src, dst ObjectRef, opts CopyOptions) (*PutResult, error) {
	info, err := c.Stat(ctx, src.Bucket, src.Key, StatOptions{VersionID: src.VersionID, SSE: opts.SourceSSE})
	if err != nil {
		return nil, err
	}
	if info.Size > MaxCopySize {
		return c.copyMultipart(ctx, src, dst, info, opts)
	}
	in := &s3.CopyObjectInput{
		Bucket:     aws.String(dst.Bucket),
		Key:        aws.String(dst.Key),
		CopySource: aws.String(CopySource(src)),
	}
	sse, srcKey := sseOf(opts.SSE), customerKeyOf(opts.SourceSSE)
	in.ServerSideEncryption, in.SSEKMSKeyId = sse.algorithm, sse.kmsKeyID
	in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = sse.customer.algorithm, sse.customer.key, sse.customer.keyMD5
	in.CopySourceSSECustomerAlgorithm, in.CopySourceSSECustomerKey, in.CopySourceSSECustomerKeyMD5 = srcKey.algorithm, srcKey.key, srcKey.keyMD5
	if opts.Checksum != domain.ChecksumNone {
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(opts.Checksum)
	}
	if opts.Metadata != nil || opts.ContentType != "" {
		in.MetadataDirective = types.MetadataDirectiveReplace
		in.Metadata = opts.Metadata
		if in.Metadata == nil {
			in.Metadata = info.Metadata
		}
		ct := opts.ContentType
		if ct == "" {
			ct = info.ContentType
		}
		if ct != "" {
			in.ContentType = aws.String(ct)
		}
	}
	if opts.Tags != nil {
		in.TaggingDirective = types.TaggingDirectiveReplace
		in.Tagging = aws.String(FormatTags(opts.Tags))
	}
	out, err := c.api.CopyObject(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, dst.Bucket, dst.Key)
	}
	if opts.Progress != nil {
		opts.Progress(info.Size)
	}
	res := &PutResult{VersionID: aws.ToString(out.VersionId), Size: info.Size}
	if r := out.CopyObjectResult; r != nil {
		res.ETag = trimETag(aws.ToString(r.ETag))
		if opts.Checksum != domain.ChecksumNone {
			res.Checksum = checksumOf(checksumValues{r.ChecksumCRC32, r.ChecksumCRC32C, r.ChecksumCRC64NVME, r.ChecksumSHA1, r.ChecksumSHA256}, r.ChecksumType)
		}
	}
	return res, nil
}

// copyMultipart copies an object larger than MaxCopySize with
// UploadPartCopy.
func (c *sdkClient) copyMultipart(ctx context.Context, src, dst ObjectRef, info *ObjectInfo, opts CopyOptions) (*PutResult, error) {
	requested := opts.PartSize
	if requested == 0 {
		requested = MaxPartSize
	}
	partSize, parts, err := PartSize(info.Size, requested)
	if err != nil {
		return nil, err
	}
	in := &s3.CreateMultipartUploadInput{Bucket: aws.String(dst.Bucket), Key: aws.String(dst.Key), Metadata: info.Metadata}
	if opts.Metadata != nil {
		in.Metadata = opts.Metadata
	}
	ct := opts.ContentType
	if ct == "" {
		ct = info.ContentType
	}
	if ct != "" {
		in.ContentType = aws.String(ct)
	}
	tags := opts.Tags
	if tags == nil {
		if tags, err = c.GetTags(ctx, src); err != nil {
			return nil, err
		}
	}
	if len(tags) > 0 {
		in.Tagging = aws.String(FormatTags(tags))
	}
	sse, srcKey := sseOf(opts.SSE), customerKeyOf(opts.SourceSSE)
	in.ServerSideEncryption, in.SSEKMSKeyId = sse.algorithm, sse.kmsKeyID
	in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5 = sse.customer.algorithm, sse.customer.key, sse.customer.keyMD5
	if opts.Checksum != domain.ChecksumNone {
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(opts.Checksum)
	}
	created, err := c.createUpload(ctx, in)
	if err != nil {
		return nil, mapS3Error(err, dst.Bucket, dst.Key)
	}
	uploadID := aws.ToString(created.UploadId)
	source := CopySource(src)
	upload := func(ctx context.Context, p part) (types.CompletedPart, error) {
		out, err := c.api.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket: aws.String(dst.Bucket), Key: aws.String(dst.Key), UploadId: aws.String(uploadID),
			PartNumber: aws.Int32(p.number), CopySource: aws.String(source),
			CopySourceRange:      aws.String(fmt.Sprintf("bytes=%d-%d", p.offset, p.offset+p.size-1)),
			SSECustomerAlgorithm: sse.customer.algorithm, SSECustomerKey: sse.customer.key, SSECustomerKeyMD5: sse.customer.keyMD5,
			CopySourceSSECustomerAlgorithm: srcKey.algorithm, CopySourceSSECustomerKey: srcKey.key, CopySourceSSECustomerKeyMD5: srcKey.keyMD5,
		})
		if err != nil {
			return types.CompletedPart{}, mapS3Error(err, dst.Bucket, dst.Key)
		}
		done := types.CompletedPart{PartNumber: aws.Int32(p.number)}
		if r := out.CopyPartResult; r != nil {
			done.ETag = r.ETag
			if opts.Checksum != domain.ChecksumNone {
				setPartChecksum(&done, checksumValues{r.ChecksumCRC32, r.ChecksumCRC32C, r.ChecksumCRC64NVME, r.ChecksumSHA1, r.ChecksumSHA256})
			}
		}
		return done, nil
	}
	ranges := &rangeParts{size: info.Size, partSize: partSize, parts: parts}
	completed, size, err := runParts(ctx, ranges, opts.Parallel, opts.Progress, upload)
	if err == nil {
		var done *s3.CompleteMultipartUploadOutput
		done, err = c.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String(dst.Bucket), Key: aws.String(dst.Key), UploadId: aws.String(uploadID),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
		})
		if err == nil {
			return &PutResult{ETag: trimETag(aws.ToString(done.ETag)), VersionID: aws.ToString(done.VersionId), Size: size, Parts: len(completed)}, nil
		}
		err = mapS3Error(err, dst.Bucket, dst.Key)
	}
	return nil, c.abort(ctx, dst.Bucket, dst.Key, uploadID, err)
}

// CopySource renders the x-amz-copy-source of an object: the bucket and
// the key URL-encoded segment by segment, and the version in the query.
func CopySource(ref ObjectRef) string {
	segments := strings.Split(ref.Key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	out := url.PathEscape(ref.Bucket) + "/" + strings.Join(segments, "/")
	if ref.VersionID != "" {
		out += "?versionId=" + url.QueryEscape(ref.VersionID)
	}
	return out
}

// Remove implements S3Client.
func (c *sdkClient) Remove(ctx context.Context, bucket string, objects []ObjectVersion, opts DeleteOptions) ([]RemoveResult, error) {
	out := make([]RemoveResult, 0, len(objects))
	for start := 0; start < len(objects); start += deleteBatch {
		batch := objects[start:min(start+deleteBatch, len(objects))]
		ids := make([]types.ObjectIdentifier, 0, len(batch))
		for _, o := range batch {
			id := types.ObjectIdentifier{Key: aws.String(o.Key)}
			if o.VersionID != "" {
				id.VersionId = aws.String(o.VersionID)
			}
			ids = append(ids, id)
		}
		in := &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(false)},
		}
		if opts.BypassGovernance {
			in.BypassGovernanceRetention = aws.Bool(true)
		}
		res, err := c.api.DeleteObjects(ctx, in)
		if err != nil {
			return out, mapS3Error(err, bucket, "")
		}
		byID := make(map[ObjectVersion]RemoveResult, len(batch))
		for _, d := range res.Deleted {
			r := RemoveResult{
				Key: aws.ToString(d.Key), VersionID: aws.ToString(d.VersionId),
				DeleteMarker: aws.ToBool(d.DeleteMarker), DeleteMarkerVersionID: aws.ToString(d.DeleteMarkerVersionId),
			}
			byID[ObjectVersion{Key: r.Key, VersionID: r.VersionID}] = r
		}
		for _, e := range res.Errors {
			r := RemoveResult{Key: aws.ToString(e.Key), VersionID: aws.ToString(e.VersionId)}
			r.Err = codeError(aws.ToString(e.Code), aws.ToString(e.Message), 0, bucket, r.Key)
			byID[ObjectVersion{Key: r.Key, VersionID: r.VersionID}] = r
		}
		for _, o := range batch {
			r, ok := byID[o]
			if !ok {
				// The server reports the deletion of the latest version
				// without the version ID.
				r, ok = byID[ObjectVersion{Key: o.Key}]
			}
			if !ok {
				r = RemoveResult{Key: o.Key, VersionID: o.VersionID}
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// ListUploads implements S3Client.
func (c *sdkClient) ListUploads(ctx context.Context, bucket, prefix string) iter.Seq2[UploadInfo, error] {
	return func(yield func(UploadInfo, error) bool) {
		in := &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)}
		if prefix != "" {
			in.Prefix = aws.String(prefix)
		}
		for {
			page, err := c.api.ListMultipartUploads(ctx, in)
			if err != nil {
				yield(UploadInfo{}, mapS3Error(err, bucket, ""))
				return
			}
			for _, u := range page.Uploads {
				info := UploadInfo{Key: aws.ToString(u.Key), UploadID: aws.ToString(u.UploadId), Initiated: aws.ToTime(u.Initiated).UTC()}
				if !yield(info, nil) {
					return
				}
			}
			if !aws.ToBool(page.IsTruncated) {
				return
			}
			nextKey, nextID := aws.ToString(page.NextKeyMarker), aws.ToString(page.NextUploadIdMarker)
			if nextKey == "" || (nextKey == aws.ToString(in.KeyMarker) && nextID == aws.ToString(in.UploadIdMarker)) {
				return
			}
			in.KeyMarker, in.UploadIdMarker = aws.String(nextKey), aws.String(nextID)
		}
	}
}

// AbortUpload implements S3Client.
func (c *sdkClient) AbortUpload(ctx context.Context, bucket, key, uploadID string) error {
	_, err := c.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	return mapS3Error(err, bucket, key)
}

// CheckExpiry validates the validity of a presigned URL or POST policy.
func CheckExpiry(d time.Duration) error {
	if d < time.Second || d > MaxPresignExpiry {
		return UsageError("expiry %s must be between 1s and %s", HumanDuration(d), HumanDuration(MaxPresignExpiry))
	}
	return nil
}

// Presign implements S3Client.
func (c *sdkClient) Presign(ctx context.Context, in PresignInput) (*PresignedRequest, error) {
	if err := CheckExpiry(in.Expiry); err != nil {
		return nil, err
	}
	if in.Bucket == "" || in.Key == "" {
		return nil, UsageError("a presigned URL needs a bucket and an object")
	}
	method := strings.ToUpper(in.Method)
	expires := s3.WithPresignExpires(in.Expiry)
	var (
		req *presignedHTTP
		err error
	)
	switch method {
	case http.MethodGet:
		if len(in.Headers) > 0 {
			return nil, UsageError("GET URLs take no headers: use --content-disposition or --content-type to override the response")
		}
		get := &s3.GetObjectInput{Bucket: aws.String(in.Bucket), Key: aws.String(in.Key)}
		if in.VersionID != "" {
			get.VersionId = aws.String(in.VersionID)
		}
		if in.ResponseContentDisposition != "" {
			get.ResponseContentDisposition = aws.String(in.ResponseContentDisposition)
		}
		if in.ResponseContentType != "" {
			get.ResponseContentType = aws.String(in.ResponseContentType)
		}
		req, err = wrapPresigned(c.presign.PresignGetObject(ctx, get, expires))
	case http.MethodHead:
		if len(in.Headers) > 0 {
			return nil, UsageError("HEAD URLs take no headers")
		}
		head := &s3.HeadObjectInput{Bucket: aws.String(in.Bucket), Key: aws.String(in.Key)}
		if in.VersionID != "" {
			head.VersionId = aws.String(in.VersionID)
		}
		req, err = wrapPresigned(c.presign.PresignHeadObject(ctx, head, expires))
	case http.MethodPut:
		put, perr := presignPutInput(in)
		if perr != nil {
			return nil, perr
		}
		opts := []func(*s3.PresignOptions){expires}
		if ct := aws.ToString(put.ContentType); ct != "" {
			// The SDK leaves Content-Type out of presigned PUTs (any type
			// is accepted); nexoctl signs the one requested.
			opts = append(opts, func(o *s3.PresignOptions) {
				o.ClientOptions = append(o.ClientOptions, func(o *s3.Options) {
					o.APIOptions = append(o.APIOptions, smithyhttp.SetHeaderValue("Content-Type", ct))
				})
			})
		}
		req, err = wrapPresigned(c.presign.PresignPutObject(ctx, put, opts...))
	case http.MethodDelete:
		if len(in.Headers) > 0 {
			return nil, UsageError("DELETE URLs take no headers")
		}
		del := &s3.DeleteObjectInput{Bucket: aws.String(in.Bucket), Key: aws.String(in.Key)}
		if in.VersionID != "" {
			del.VersionId = aws.String(in.VersionID)
		}
		req, err = wrapPresigned(c.presign.PresignDeleteObject(ctx, del, expires))
	default:
		return nil, UsageError("unsupported method %q (GET, HEAD, PUT, DELETE)", in.Method)
	}
	if err != nil {
		return nil, mapS3Error(err, in.Bucket, in.Key)
	}
	headers := map[string]string{}
	for k, vs := range req.headers {
		if strings.EqualFold(k, "Host") || len(vs) == 0 {
			continue
		}
		headers[http.CanonicalHeaderKey(k)] = strings.Join(vs, ",")
	}
	return &PresignedRequest{Method: method, URL: req.url, Headers: headers, ExpiresAt: c.clock.Now().UTC().Add(in.Expiry).Truncate(time.Second)}, nil
}

// presignedHTTP is the part of a v4.PresignedHTTPRequest nexoctl uses.
type presignedHTTP struct {
	url     string
	headers http.Header
}

func wrapPresigned(req *v4.PresignedHTTPRequest, err error) (*presignedHTTP, error) {
	if err != nil {
		return nil, err
	}
	return &presignedHTTP{url: req.URL, headers: req.SignedHeader}, nil
}

// presignPutInput maps the signed headers of a presigned PUT onto the
// PutObject input.
func presignPutInput(in PresignInput) (*s3.PutObjectInput, error) {
	put := &s3.PutObjectInput{Bucket: aws.String(in.Bucket), Key: aws.String(in.Key)}
	for _, k := range SortedKeys(in.Headers) {
		v := in.Headers[k]
		switch lk := strings.ToLower(k); {
		case lk == "content-type":
			put.ContentType = aws.String(v)
		case lk == "content-disposition":
			put.ContentDisposition = aws.String(v)
		case lk == "cache-control":
			put.CacheControl = aws.String(v)
		case lk == "content-encoding":
			put.ContentEncoding = aws.String(v)
		case lk == "content-language":
			put.ContentLanguage = aws.String(v)
		case strings.HasPrefix(lk, "x-amz-meta-") && len(lk) > len("x-amz-meta-"):
			if put.Metadata == nil {
				put.Metadata = map[string]string{}
			}
			put.Metadata[strings.TrimPrefix(lk, "x-amz-meta-")] = v
		default:
			return nil, UsageError("header %q cannot be presigned (Content-Type, Content-Disposition, Cache-Control, Content-Encoding, Content-Language, x-amz-meta-*)", k)
		}
	}
	return put, nil
}

// PostPolicy implements S3Client: the policy document is signed locally
// with SigV4 (docs/09 §9.2).
func (c *sdkClient) PostPolicy(_ context.Context, in PostPolicyInput) (*PostPolicyForm, error) {
	if err := CheckExpiry(in.Expiry); err != nil {
		return nil, err
	}
	if in.Bucket == "" {
		return nil, UsageError("a POST policy needs a bucket")
	}
	if in.MaxSize < 0 || in.MinSize < 0 || (in.MaxSize > 0 && in.MinSize > in.MaxSize) {
		return nil, UsageError("invalid size range %d-%d", in.MinSize, in.MaxSize)
	}
	now := c.clock.Now().UTC()
	date := now.Format("20060102T150405Z")
	scope := now.Format("20060102") + "/" + c.region + "/s3/aws4_request"
	credential := c.alias.AccessKey + "/" + scope
	key := in.Key
	conditions := []any{map[string]string{"bucket": in.Bucket}}
	if key != "" {
		conditions = append(conditions, map[string]string{"key": key})
	} else {
		conditions = append(conditions, []any{"starts-with", "$key", in.KeyPrefix})
		key = in.KeyPrefix + "${filename}"
	}
	conditions = append(conditions,
		map[string]string{"x-amz-algorithm": "AWS4-HMAC-SHA256"},
		map[string]string{"x-amz-credential": credential},
		map[string]string{"x-amz-date": date},
	)
	if in.MaxSize > 0 {
		conditions = append(conditions, []any{"content-length-range", in.MinSize, in.MaxSize})
	}
	if in.ContentType != "" {
		conditions = append(conditions, map[string]string{"Content-Type": in.ContentType})
	}
	expiresAt := now.Add(in.Expiry).Truncate(time.Second)
	doc, err := json.Marshal(map[string]any{
		"expiration": expiresAt.Format("2006-01-02T15:04:05.000Z"),
		"conditions": conditions,
	})
	if err != nil {
		return nil, fmt.Errorf("encode POST policy: %w", err)
	}
	policy := base64.StdEncoding.EncodeToString(doc)
	signingKey := hmacSHA256([]byte("AWS4"+c.alias.SecretKey), now.Format("20060102"))
	for _, s := range []string{c.region, "s3", "aws4_request"} {
		signingKey = hmacSHA256(signingKey, s)
	}
	fields := map[string]string{
		"key":              key,
		"policy":           policy,
		"x-amz-algorithm":  "AWS4-HMAC-SHA256",
		"x-amz-credential": credential,
		"x-amz-date":       date,
		"x-amz-signature":  hex.EncodeToString(hmacSHA256(signingKey, policy)),
	}
	if in.ContentType != "" {
		fields["Content-Type"] = in.ContentType
	}
	return &PostPolicyForm{URL: c.bucketURL(in.Bucket), Fields: fields, ExpiresAt: expiresAt}, nil
}

// bucketURL is the URL of a bucket in the style of the alias.
func (c *sdkClient) bucketURL(bucket string) string {
	u := *c.endpoint
	if c.pathStyle {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + bucket
	} else {
		u.Host = bucket + "." + u.Host
	}
	return u.String()
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// GetBucketPolicy implements S3Client.
func (c *sdkClient) GetBucketPolicy(ctx context.Context, bucket string) (string, error) {
	out, err := c.api.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil {
		return "", mapS3Error(err, bucket, "")
	}
	return aws.ToString(out.Policy), nil
}

// PutBucketPolicy implements S3Client.
func (c *sdkClient) PutBucketPolicy(ctx context.Context, bucket, policy string) error {
	_, err := c.api.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), Policy: aws.String(policy)})
	return mapS3Error(err, bucket, "")
}

// DeleteBucketPolicy implements S3Client.
func (c *sdkClient) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	_, err := c.api.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)})
	return mapS3Error(err, bucket, "")
}

// GetVersioning implements S3Client.
func (c *sdkClient) GetVersioning(ctx context.Context, bucket string) (Versioning, error) {
	out, err := c.api.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return VersioningOff, mapS3Error(err, bucket, "")
	}
	return Versioning(out.Status), nil
}

// SetVersioning implements S3Client.
func (c *sdkClient) SetVersioning(ctx context.Context, bucket string, v Versioning) error {
	if v != VersioningEnabled && v != VersioningSuspended {
		return UsageError("invalid versioning state %q (Enabled, Suspended)", v)
	}
	_, err := c.api.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatus(v)},
	})
	return mapS3Error(err, bucket, "")
}

// GetTags implements S3Client.
func (c *sdkClient) GetTags(ctx context.Context, ref ObjectRef) (map[string]string, error) {
	var set []types.Tag
	if ref.Key == "" {
		out, err := c.api.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(ref.Bucket)})
		if err != nil {
			if isCode(err, "NoSuchTagSet", "NoSuchTagSetError") {
				return map[string]string{}, nil
			}
			return nil, mapS3Error(err, ref.Bucket, "")
		}
		set = out.TagSet
	} else {
		in := &s3.GetObjectTaggingInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key)}
		if ref.VersionID != "" {
			in.VersionId = aws.String(ref.VersionID)
		}
		out, err := c.api.GetObjectTagging(ctx, in)
		if err != nil {
			return nil, mapS3Error(err, ref.Bucket, ref.Key)
		}
		set = out.TagSet
	}
	tags := make(map[string]string, len(set))
	for _, t := range set {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

// PutTags implements S3Client.
func (c *sdkClient) PutTags(ctx context.Context, ref ObjectRef, tags map[string]string) error {
	set := make([]types.Tag, 0, len(tags))
	for _, k := range SortedKeys(tags) {
		set = append(set, types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	if ref.Key == "" {
		_, err := c.api.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(ref.Bucket), Tagging: &types.Tagging{TagSet: set}})
		return mapS3Error(err, ref.Bucket, "")
	}
	in := &s3.PutObjectTaggingInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key), Tagging: &types.Tagging{TagSet: set}}
	if ref.VersionID != "" {
		in.VersionId = aws.String(ref.VersionID)
	}
	_, err := c.api.PutObjectTagging(ctx, in)
	return mapS3Error(err, ref.Bucket, ref.Key)
}

// DeleteTags implements S3Client.
func (c *sdkClient) DeleteTags(ctx context.Context, ref ObjectRef) error {
	if ref.Key == "" {
		_, err := c.api.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: aws.String(ref.Bucket)})
		return mapS3Error(err, ref.Bucket, "")
	}
	in := &s3.DeleteObjectTaggingInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key)}
	if ref.VersionID != "" {
		in.VersionId = aws.String(ref.VersionID)
	}
	_, err := c.api.DeleteObjectTagging(ctx, in)
	return mapS3Error(err, ref.Bucket, ref.Key)
}

// trimETag removes the quotes of an ETag.
func trimETag(s string) string { return strings.Trim(s, `"`) }

// lowerKeys returns the metadata with lower-case keys (nil for none).
func lowerKeys(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// readerAt returns the io.ReaderAt of a body that supports random access:
// a regular file or a reader that implements it (bytes.Reader, ...).
func readerAt(body io.Reader) (io.ReaderAt, bool) {
	if f, ok := body.(*os.File); ok {
		st, err := f.Stat()
		return f, err == nil && st.Mode().IsRegular()
	}
	ra, ok := body.(io.ReaderAt)
	return ra, ok
}

// sizedBody returns a seekable reader over the size bytes of body: a
// section of a random-access body, or a copy in memory (size is at most
// one part).
func sizedBody(body io.Reader, size int64) (io.ReadSeeker, error) {
	if ra, ok := readerAt(body); ok {
		return io.NewSectionReader(ra, 0, size), nil
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(body, buf); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return bytes.NewReader(buf), nil
}
