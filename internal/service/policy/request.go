package policy

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Request condition keys (RequestConditions). Keys are case-insensitive
// for the engine; these are their canonical spellings.
const (
	KeySourceIP             = "aws:SourceIp"
	KeySecureTransport      = "aws:SecureTransport"
	KeyUserAgent            = "aws:UserAgent"
	KeyReferer              = "aws:Referer"
	KeyCurrentTime          = "aws:CurrentTime"
	KeyEpochTime            = "aws:EpochTime"
	KeyPrefix               = "s3:prefix"
	KeyDelimiter            = "s3:delimiter"
	KeyMaxKeys              = "s3:max-keys"
	KeyVersionID            = "s3:versionid"
	KeyAuthType             = "s3:authType"
	KeySignatureVersion     = "s3:signatureversion"
	KeySignatureAge         = "s3:signatureAge"
	KeyRequestObjectTagKeys = "s3:RequestObjectTagKeys"
	// KeyRequestObjectTagPrefix is followed by the tag key.
	KeyRequestObjectTagPrefix = "s3:RequestObjectTag/"
)

// Request headers with a meaning for the condition keys.
const (
	// HeaderTagging is the x-amz-tagging header: the URL-encoded tags of
	// an upload (s3:RequestObjectTag/<key>).
	HeaderTagging = "x-amz-tagging"
	// SignatureVersion is the s3:signatureversion of a SigV4 request.
	SignatureVersion = "AWS4-HMAC-SHA256"
	headerAmzPrefix  = "x-amz-"
	headerMetaPrefix = "x-amz-meta-"
	headerToken      = "x-amz-security-token"
)

// requestParams maps the query parameters of Request.Params to their
// condition keys.
func requestParams() map[string]string {
	return map[string]string{"prefix": KeyPrefix, "delimiter": KeyDelimiter, "max-keys": KeyMaxKeys, "versionId": KeyVersionID}
}

// RequestConditions returns the condition values of a request (docs/09
// §9.3): the aws:* keys of the client and of the time, the s3:* keys of
// the listing parameters and of the version, every x-amz-* header as
// s3:x-amz-* (x-amz-meta-* and x-amz-security-token aside), the tags
// requested with x-amz-tagging (s3:RequestObjectTag/<key>, sorted
// s3:RequestObjectTagKeys; a malformed value gives no tag key, the
// operation rejects it), the authentication type and the signature age.
// aws:username, aws:userid and the other identity keys are added by the
// engine (Args.Conditions). The result is a new map.
func RequestConditions(r Request) map[string][]string {
	conds := map[string][]string{}
	set := func(k, v string) {
		if v != "" {
			conds[k] = []string{v}
		}
	}
	if c := r.Client; c != nil {
		set(KeySourceIP, c.IP)
		set(KeySecureTransport, strconv.FormatBool(c.Secure))
		set(KeyUserAgent, c.UserAgent)
		set(KeyReferer, c.Referer)
	}
	if !r.Now.IsZero() {
		set(KeyCurrentTime, r.Now.UTC().Format(time.RFC3339))
		set(KeyEpochTime, strconv.FormatInt(r.Now.Unix(), 10))
	}
	keys := requestParams()
	for name, value := range r.Params {
		if key, ok := keys[name]; ok {
			conds[key] = []string{value}
		}
	}
	tagging := ""
	for name, values := range r.Headers {
		lname := strings.ToLower(strings.TrimSpace(name))
		if !strings.HasPrefix(lname, headerAmzPrefix) || strings.HasPrefix(lname, headerMetaPrefix) || lname == headerToken || len(values) == 0 {
			continue
		}
		conds["s3:"+lname] = append([]string(nil), values...)
		if lname == HeaderTagging {
			tagging = values[0]
		}
	}
	if tags, err := url.ParseQuery(tagging); err == nil && len(tags) > 0 {
		names := make([]string, 0, len(tags))
		for k, vs := range tags {
			names = append(names, k)
			if len(vs) > 0 {
				conds[KeyRequestObjectTagPrefix+k] = []string{vs[0]}
			}
		}
		sort.Strings(names)
		conds[KeyRequestObjectTagKeys] = names
	}
	if r.AuthType != "" {
		set(KeyAuthType, string(r.AuthType))
		set(KeySignatureVersion, SignatureVersion)
	}
	if !r.SignedAt.IsZero() && !r.Now.IsZero() {
		set(KeySignatureAge, strconv.FormatInt(r.Now.Sub(r.SignedAt).Milliseconds(), 10))
	}
	return conds
}
