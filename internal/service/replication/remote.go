package replication

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Content-MD5 is the integrity check of the S3 API
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/r3n3r0/nexo/internal/domain"
)

// s3Dialer opens S3 clients of the remote buckets.
type s3Dialer struct {
	client *http.Client
}

var _ Dialer = s3Dialer{}

// NewS3Dialer returns the Dialer of the S3 API (path-style requests,
// SigV4). client nil uses a client with connection and response timeouts
// and no overall one (a large version takes its time).
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
		RetryMaxAttempts:           2,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &s3Remote{client: c, bucket: cfg.Bucket}, nil
}

var _ Remote = (*s3Remote)(nil)

// s3Remote is the Remote of a bucket reached through the S3 API.
type s3Remote struct {
	client *s3.Client
	bucket string
}

// headers adds request headers to one call.
func headers(kv ...string) func(*s3.Options) {
	return func(o *s3.Options) {
		for i := 0; i+1 < len(kv); i += 2 {
			o.APIOptions = append(o.APIOptions, smithyhttp.AddHeaderValue(kv[i], kv[i+1]))
		}
	}
}

// replicaHeaders are the headers of the write of a version.
func replicaHeaders(v RemotePut) func(*s3.Options) {
	kv := []string{domain.HeaderReplica, "true", domain.HeaderReplicaModTime, v.ModTime.UTC().Format(domain.ReplicaModTimeLayout)}
	if v.VersionID != "" && v.VersionID != "null" {
		kv = append(kv, domain.HeaderReplicaVersionID, v.VersionID)
	}
	if v.ETag != "" {
		kv = append(kv, domain.HeaderReplicaETag, v.ETag)
	}
	return headers(kv...)
}

// CheckBucket implements Remote.
func (r *s3Remote) CheckBucket(ctx context.Context) error {
	out, err := r.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(r.bucket)})
	if err != nil {
		return err
	}
	if out.Status != types.BucketVersioningStatusEnabled {
		return fmt.Errorf("versioning is not enabled on the remote bucket %s", r.bucket)
	}
	return nil
}

// Head implements Remote.
func (r *s3Remote) Head(ctx context.Context, key, versionID string) (RemoteVersion, error) {
	in := &s3.HeadObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	out, err := r.client.HeadObject(ctx, in)
	if err != nil {
		var re *smithyhttp.ResponseError
		if errors.As(err, &re) && (re.HTTPStatusCode() == http.StatusNotFound || re.HTTPStatusCode() == http.StatusMethodNotAllowed) {
			return RemoteVersion{}, nil
		}
		return RemoteVersion{}, err
	}
	return RemoteVersion{Exists: true, ETag: trimETag(aws.ToString(out.ETag)), Size: aws.ToInt64(out.ContentLength)}, nil
}

func trimETag(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// encodeTags encodes tags as the x-amz-tagging header.
func encodeTags(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	q := url.Values{}
	for _, k := range keys {
		q.Set(k, tags[k])
	}
	return q.Encode()
}

// Put implements Remote.
func (r *s3Remote) Put(ctx context.Context, v RemotePut, data []byte) error {
	sum := md5.Sum(data) // #nosec G401 -- Content-MD5
	in := &s3.PutObjectInput{
		Bucket: aws.String(r.bucket), Key: aws.String(v.Key), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))), ContentMD5: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	}
	applyPut(v, putFields{
		contentType: &in.ContentType, contentEncoding: &in.ContentEncoding, contentDisposition: &in.ContentDisposition,
		contentLanguage: &in.ContentLanguage, cacheControl: &in.CacheControl, expires: &in.Expires, metadata: &in.Metadata,
		tagging: &in.Tagging, lockMode: &in.ObjectLockMode, retainUntil: &in.ObjectLockRetainUntilDate, legalHold: &in.ObjectLockLegalHoldStatus,
		sse: &in.ServerSideEncryption, kmsKey: &in.SSEKMSKeyId,
	})
	_, err := r.client.PutObject(ctx, in, replicaHeaders(v))
	return err
}

// putFields points to the fields of a PutObject or CreateMultipartUpload
// input.
type putFields struct {
	contentType, contentEncoding, contentDisposition, contentLanguage, cacheControl **string
	expires                                                                         **time.Time
	metadata                                                                        *map[string]string
	tagging                                                                         **string
	lockMode                                                                        *types.ObjectLockMode
	retainUntil                                                                     **time.Time
	legalHold                                                                       *types.ObjectLockLegalHoldStatus
	sse                                                                             *types.ServerSideEncryption
	kmsKey                                                                          **string
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// applyPut sets the metadata of v on an input.
func applyPut(v RemotePut, f putFields) {
	*f.contentType = optString(v.ContentType)
	*f.contentEncoding = optString(v.ContentEncoding)
	*f.contentDisposition = optString(v.ContentDisposition)
	*f.contentLanguage = optString(v.ContentLanguage)
	*f.cacheControl = optString(v.CacheControl)
	if !v.Expires.IsZero() {
		*f.expires = aws.Time(v.Expires)
	}
	if len(v.UserMetadata) > 0 {
		*f.metadata = v.UserMetadata
	}
	if len(v.Tags) > 0 {
		*f.tagging = aws.String(encodeTags(v.Tags))
	}
	if v.Retention.Mode != "" && !v.Retention.RetainUntil.IsZero() {
		*f.lockMode = types.ObjectLockMode(v.Retention.Mode)
		*f.retainUntil = aws.Time(v.Retention.RetainUntil)
	}
	if v.LegalHold != "" {
		*f.legalHold = types.ObjectLockLegalHoldStatus(v.LegalHold)
	}
	switch v.Encryption.Type {
	case domain.SSES3:
		*f.sse = types.ServerSideEncryptionAes256
	case domain.SSEKMS:
		*f.sse = types.ServerSideEncryptionAwsKms
		*f.kmsKey = optString(v.Encryption.KMSKeyID)
	}
}

// CreateUpload implements Remote.
func (r *s3Remote) CreateUpload(ctx context.Context, v RemotePut) (string, error) {
	in := &s3.CreateMultipartUploadInput{Bucket: aws.String(r.bucket), Key: aws.String(v.Key)}
	applyPut(v, putFields{
		contentType: &in.ContentType, contentEncoding: &in.ContentEncoding, contentDisposition: &in.ContentDisposition,
		contentLanguage: &in.ContentLanguage, cacheControl: &in.CacheControl, expires: &in.Expires, metadata: &in.Metadata,
		tagging: &in.Tagging, lockMode: &in.ObjectLockMode, retainUntil: &in.ObjectLockRetainUntilDate, legalHold: &in.ObjectLockLegalHoldStatus,
		sse: &in.ServerSideEncryption, kmsKey: &in.SSEKMSKeyId,
	})
	out, err := r.client.CreateMultipartUpload(ctx, in, replicaHeaders(v))
	if err != nil {
		return "", err
	}
	return aws.ToString(out.UploadId), nil
}

// UploadPart implements Remote.
func (r *s3Remote) UploadPart(ctx context.Context, key, uploadID string, number int, data []byte) (string, error) {
	sum := md5.Sum(data) // #nosec G401 -- Content-MD5
	out, err := r.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(r.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(int32(number)), // #nosec G115 -- at most 10000 parts
		Body:       bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))),
		ContentMD5: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	}, headers(domain.HeaderReplica, "true"))
	if err != nil {
		return "", err
	}
	return trimETag(aws.ToString(out.ETag)), nil
}

// CompleteUpload implements Remote.
func (r *s3Remote) CompleteUpload(ctx context.Context, key, uploadID string, parts []RemotePart) error {
	done := &types.CompletedMultipartUpload{}
	for _, p := range parts {
		done.Parts = append(done.Parts, types.CompletedPart{
			PartNumber: aws.Int32(int32(p.Number)), // #nosec G115 -- at most 10000 parts
			ETag:       aws.String(strconv.Quote(p.ETag)),
		})
	}
	_, err := r.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(r.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), MultipartUpload: done,
	}, headers(domain.HeaderReplica, "true"))
	return err
}

// AbortUpload implements Remote.
func (r *s3Remote) AbortUpload(ctx context.Context, key, uploadID string) error {
	_, err := r.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(r.bucket), Key: aws.String(key), UploadId: aws.String(uploadID)},
		headers(domain.HeaderReplica, "true"))
	return err
}

// DeleteMarker implements Remote.
func (r *s3Remote) DeleteMarker(ctx context.Context, key, versionID string, modTime time.Time) error {
	kv := []string{domain.HeaderReplica, "true", domain.HeaderReplicaDeleteMarker, "true"}
	if versionID != "" && versionID != "null" {
		kv = append(kv, domain.HeaderReplicaVersionID, versionID)
	}
	if !modTime.IsZero() {
		kv = append(kv, domain.HeaderReplicaModTime, modTime.UTC().Format(domain.ReplicaModTimeLayout))
	}
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)}, headers(kv...))
	return err
}

// DeleteVersion implements Remote.
func (r *s3Remote) DeleteVersion(ctx context.Context, key, versionID string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key), VersionId: aws.String(versionID)},
		headers(domain.HeaderReplica, "true"))
	return err
}

// PutTags implements Remote.
func (r *s3Remote) PutTags(ctx context.Context, key, versionID string, tags map[string]string) error {
	set := types.Tagging{TagSet: []types.Tag{}}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		set.TagSet = append(set.TagSet, types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	in := &s3.PutObjectTaggingInput{Bucket: aws.String(r.bucket), Key: aws.String(key), Tagging: &set}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	_, err := r.client.PutObjectTagging(ctx, in, headers(domain.HeaderReplica, "true"))
	return err
}
