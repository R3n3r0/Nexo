package nexoadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// maxResponse bounds the JSON responses read by the client.
const maxResponse = 64 << 20

// signer signs the requests with SigV4 (service s3).
type signer struct {
	v4     *v4.Signer
	creds  aws.Credentials
	region string
	cfg    Config
}

func newSigner(cfg Config) *signer {
	return &signer{
		// Like S3, the Admin API signs the path as sent (no double
		// encoding).
		v4:     v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }),
		creds:  aws.Credentials{AccessKeyID: cfg.AccessKey, SecretAccessKey: cfg.SecretKey, SessionToken: cfg.SessionToken},
		region: cfg.Region,
		cfg:    cfg,
	}
}

// sign adds the SigV4 headers to req, whose body is payload.
func (s *signer) sign(ctx context.Context, req *http.Request, payload []byte) error {
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	return s.v4.SignHTTP(ctx, s.creds, req, hash, "s3", s.region, s.cfg.Now().UTC())
}

// request describes a call of the Admin API.
type request struct {
	method string
	path   string
	query  url.Values
	// body is encoded as JSON unless raw is set.
	body any
	raw  []byte
	// contentType of raw bodies.
	contentType string
}

// pathOf joins escaped path segments: each segment is escaped once, so a
// "/" inside a name stays inside its segment ("%2F").
func pathOf(segments ...string) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// endpoint returns the URL of an Admin API path, escaped already (pathOf).
// Both forms of the path are set so that the URL is sent, and signed
// (DisableURIPathEscaping signs EscapedPath), exactly as escaped: setting
// only the decoded Path would let url.URL escape the escapes again ("%20"
// sent as "%2520"), and clearing RawPath would turn an escaped "/" into a
// path separator.
func (c *client) endpoint(path string) (url.URL, error) {
	u := *c.base
	raw := strings.TrimSuffix(c.base.EscapedPath(), "/") + path
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return url.URL{}, fmt.Errorf("nexoadmin: invalid path %q: %w", path, err)
	}
	u.Path, u.RawPath = decoded, raw
	return u, nil
}

// do sends a request and decodes a JSON response into out (nil: none).
func (c *client) do(ctx context.Context, in request, out any) error {
	data, _, err := c.send(ctx, in)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("nexoadmin: %s %s: decode response: %w", in.method, in.path, err)
	}
	return nil
}

// send performs a request and returns the body of a successful response.
func (c *client) send(ctx context.Context, in request) ([]byte, http.Header, error) {
	payload := in.raw
	contentType := in.contentType
	if in.body != nil {
		var err error
		if payload, err = json.Marshal(in.body); err != nil {
			return nil, nil, fmt.Errorf("nexoadmin: encode request: %w", err)
		}
		contentType = "application/json"
	}
	u, err := c.endpoint(in.path)
	if err != nil {
		return nil, nil, err
	}
	if len(in.query) > 0 {
		u.RawQuery = in.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, in.method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("nexoadmin: %w", err)
	}
	if payload == nil {
		req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if err := c.signer.sign(ctx, req, payload); err != nil {
		return nil, nil, fmt.Errorf("nexoadmin: sign request: %w", err)
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("nexoadmin: %s %s: %w", in.method, in.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, nil, fmt.Errorf("nexoadmin: %s %s: read response: %w", in.method, in.path, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, nil, newAPIError(resp, data)
	}
	return data, resp.Header, nil
}

// APIError is an error answered by the Admin API: an RFC 9457 problem
// document with a stable Code.
type APIError struct {
	// Status is the HTTP status.
	Status int `json:"status"`
	// Code is the stable error code (access_denied, user_not_found...).
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// RequestID correlates the error with the server logs.
	RequestID string `json:"requestId"`
	// Errors locate the problems of a malformed policy document
	// (Code malformed_policy).
	Errors []PolicyError `json:"errors"`
}

// Error implements error.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("nexoadmin: %d %s", e.Status, e.Code)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	for _, pe := range e.Errors {
		if pe.Line > 0 {
			msg += fmt.Sprintf(" (line %d, column %d)", pe.Line, pe.Column)
		}
	}
	return msg
}

// newAPIError decodes an error response; bodies that are not problem
// documents keep the status and its text.
func newAPIError(resp *http.Response, data []byte) *APIError {
	e := &APIError{}
	if err := json.Unmarshal(data, e); err != nil || e.Code == "" {
		e = &APIError{Code: strings.ToLower(strings.ReplaceAll(http.StatusText(resp.StatusCode), " ", "_")), Detail: strings.TrimSpace(string(data))}
	}
	e.Status = resp.StatusCode
	if e.Title == "" {
		e.Title = http.StatusText(resp.StatusCode)
	}
	return e
}

// Error codes of the Admin API checked by the helpers.
const (
	CodeAccessDenied    = "access_denied"
	CodeUnauthenticated = "unauthenticated"
	CodeMalformedPolicy = "malformed_policy"
	CodeInvalidArgument = "invalid_argument"
	CodeConflict        = "conflict"
)

// IsNotFound reports an error answered with 404 (unknown user, policy,
// bucket, object, share...).
func IsNotFound(err error) bool { return statusOf(err) == http.StatusNotFound }

// IsAccessDenied reports an error answered with 403.
func IsAccessDenied(err error) bool { return statusOf(err) == http.StatusForbidden }

// IsUnauthenticated reports an error answered with 401 (unknown access
// key, wrong signature).
func IsUnauthenticated(err error) bool { return statusOf(err) == http.StatusUnauthorized }

// IsConflict reports an error answered with 409 (already exists, not
// empty).
func IsConflict(err error) bool { return statusOf(err) == http.StatusConflict }

func statusOf(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// listQuery encodes the paging options.
func listQuery(o ListOptions, q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	return q
}
