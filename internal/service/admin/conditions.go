package admin

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// HeaderTagging is the x-amz-tagging header: the URL-encoded tags of an
// upload.
const HeaderTagging = policy.HeaderTagging

// ClientConditions returns the condition values every action of an Admin
// API request is evaluated with: aws:SourceIp, aws:SecureTransport,
// aws:UserAgent and aws:Referer of the client, plus the s3:authType
// (REST-HEADER) and s3:signatureversion of a request signed in its
// headers. The console stands for such a client: a policy that denies the
// presigned URLs or the anonymous requests does not deny it, one that
// requires signed requests allows it, as through the S3 API. The keys are
// those of the S3 API (policy.RequestConditions).
func ClientConditions(c Client) map[string][]string {
	return policy.RequestConditions(policy.Request{
		Client:   &policy.RequestClient{IP: c.IP, Secure: c.Secure, UserAgent: c.UserAgent, Referer: c.Referer},
		AuthType: domain.AuthTypeHeader,
	})
}

// ObjectConditions returns the condition values specific to the S3 request
// an object operation stands for (see ObjectRequest), to merge over the
// ones of the requester: s3:versionid, the forwarded x-amz-* headers, the
// requested tags and the authentication type of presigned URLs and POST
// policies. They are computed by the same definition as those of the S3
// API (policy.RequestConditions), so a policy decides alike through both.
// A malformed x-amz-tagging gives no tag condition (the operation itself
// rejects it, ParseTagging).
func ObjectConditions(in ObjectRequest) map[string][]string {
	r := policy.Request{AuthType: in.AuthType}
	if in.VersionID != "" {
		r.Params = map[string]string{"versionId": in.VersionID}
	}
	if len(in.Headers) > 0 {
		r.Headers = make(map[string][]string, len(in.Headers))
		for name, value := range in.Headers {
			r.Headers[name] = []string{value}
		}
	}
	return policy.RequestConditions(r)
}

// ListingConditions returns the condition values of a listing of a bucket
// (s3:prefix, s3:delimiter when set and, when limit is positive,
// s3:max-keys), as the S3 API computes them for ListObjects.
func ListingConditions(prefix, delimiter string, limit int) map[string][]string {
	params := map[string]string{"prefix": prefix}
	if delimiter != "" {
		params["delimiter"] = delimiter
	}
	if limit > 0 {
		params["max-keys"] = strconv.Itoa(limit)
	}
	return policy.RequestConditions(policy.Request{Params: params})
}

// copySourceOf is the x-amz-copy-source header of a CopyObject of
// bucket/key (a version of it when versionID is set), as the S3 clients
// send it: the key URL-encoded segment by segment.
func copySourceOf(bucket, key, versionID string) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	out := bucket + "/" + strings.Join(segments, "/")
	if versionID != "" {
		out += "?versionId=" + url.QueryEscape(versionID)
	}
	return out
}

// ParseTagging decodes an x-amz-tagging value (URL-encoded tags, "" for
// none) as the S3 API does: a malformed value or a repeated key is
// domain.ErrInvalidArgument.
func ParseTagging(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be URL-encoded tags", domain.ErrInvalidArgument, HeaderTagging)
	}
	tags := make(map[string]string, len(values))
	for k, vs := range values {
		if len(vs) > 1 {
			return nil, fmt.Errorf("%w: %s repeats the tag %q", domain.ErrInvalidArgument, HeaderTagging, k)
		}
		tags[k] = vs[0]
	}
	return tags, nil
}

// authorizeObject authorizes actions on one object with the condition
// values of the S3 request the operation stands for (ObjectConditions),
// and turns a denial into an error wrapping domain.ErrAccessDenied.
func authorizeObject(ctx context.Context, authz Authorizer, req Requester, bucket, key string, or ObjectRequest, actions ...policy.Action) error {
	d, err := authz.Authorize(ctx, AuthorizeInput{
		Requester: req, Actions: actions, Bucket: bucket, Object: key, Conditions: ObjectConditions(or),
	})
	if err != nil {
		return err
	}
	if !d.Allowed {
		return denied(d)
	}
	return nil
}
