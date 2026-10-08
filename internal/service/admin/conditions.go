package admin

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Names of the headers and condition keys shared with the S3 API
// (transport/http/s3 requestConditions).
const (
	// HeaderTagging is the x-amz-tagging header: the URL-encoded tags of
	// an upload.
	HeaderTagging = "x-amz-tagging"
	// signatureVersion is the s3:signatureversion of a SigV4 request.
	signatureVersion = "AWS4-HMAC-SHA256"
)

// ClientConditions returns the condition values every action of an Admin
// API request is evaluated with: aws:SourceIp, aws:SecureTransport,
// aws:UserAgent and aws:Referer of the client, plus the s3:authType
// (REST-HEADER) and s3:signatureversion of a request signed in its
// headers. The console stands for such a client: a policy that denies the
// presigned URLs or the anonymous requests does not deny it, one that
// requires signed requests allows it, as through the S3 API.
func ClientConditions(c Client) map[string][]string {
	conds := map[string][]string{
		"aws:SecureTransport": {strconv.FormatBool(c.Secure)},
		"s3:authType":         {string(domain.AuthTypeHeader)},
		"s3:signatureversion": {signatureVersion},
	}
	for k, v := range map[string]string{"aws:SourceIp": c.IP, "aws:UserAgent": c.UserAgent, "aws:Referer": c.Referer} {
		if v != "" {
			conds[k] = []string{v}
		}
	}
	return conds
}

// ObjectConditions returns the condition values specific to the S3 request
// an object operation stands for (see ObjectRequest), to merge over the
// ones of the requester: s3:versionid, the forwarded x-amz-* headers, the
// requested tags and the authentication type of presigned URLs and POST
// policies. A malformed x-amz-tagging gives no tag condition (the
// operation itself rejects it, ParseTagging).
func ObjectConditions(in ObjectRequest) map[string][]string {
	conds := map[string][]string{}
	if in.VersionID != "" {
		conds["s3:versionid"] = []string{in.VersionID}
	}
	tagging := ""
	for name, value := range in.Headers {
		lname := strings.ToLower(strings.TrimSpace(name))
		if !strings.HasPrefix(lname, "x-amz-") || strings.HasPrefix(lname, "x-amz-meta-") || lname == "x-amz-security-token" {
			continue
		}
		conds["s3:"+lname] = []string{value}
		if lname == HeaderTagging {
			tagging = value
		}
	}
	if tags, err := url.ParseQuery(tagging); err == nil && len(tags) > 0 {
		keys := make([]string, 0, len(tags))
		for k, vs := range tags {
			keys = append(keys, k)
			if len(vs) > 0 {
				conds["s3:RequestObjectTag/"+k] = []string{vs[0]}
			}
		}
		sort.Strings(keys)
		conds["s3:RequestObjectTagKeys"] = keys
	}
	if in.AuthType != "" {
		conds["s3:authType"] = []string{string(in.AuthType)}
	}
	return conds
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
