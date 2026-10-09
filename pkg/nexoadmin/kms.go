package nexoadmin

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// KMS backends (KMSStatus.Backend).
const (
	// KMSBackendBuiltin is the builtin KMS: a master key from the
	// environment (NEXO_KMS_SECRET_KEY).
	KMSBackendBuiltin = "builtin"
	// KMSBackendKES is an external KES server (NEXO_KMS_ENDPOINT).
	KMSBackendKES = "kes"
)

// KMSStatus is the state of the KMS of the server-side encryption.
type KMSStatus struct {
	// Configured is false without KMS: SSE-S3 and SSE-KMS are refused,
	// SSE-C still works.
	Configured bool `json:"configured"`
	// Backend is KMSBackendBuiltin or KMSBackendKES.
	Backend string `json:"backend,omitempty"`
	// DefaultKeyID is the key of SSE-S3 and of the SSE-KMS requests
	// without key id.
	DefaultKeyID string `json:"defaultKeyId,omitempty"`
	// Endpoints are the servers of an external KMS.
	Endpoints []KMSEndpoint `json:"endpoints,omitempty"`
	// Online reports that the KMS answers; Error why it does not.
	Online  bool   `json:"online"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
}

// KMSEndpoint is a server of an external KMS.
type KMSEndpoint struct {
	URL    string `json:"url"`
	Online bool   `json:"online"`
}

// KMSKey is a key of the KMS.
type KMSKey struct {
	ID        string     `json:"id"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	// Default marks the key of SSE-S3.
	Default bool `json:"default,omitempty"`
}

// KMSResealResult is the outcome of ResealKMSKeys.
type KMSResealResult struct {
	// Buckets counts the buckets walked.
	Buckets int `json:"buckets"`
	// Versions counts the SSE-S3 and SSE-KMS versions met, Uploads the
	// pending multipart uploads of such objects; Resealed counts the
	// versions and uploads re-sealed under the current master key, Failed
	// those that could not be (FirstError tells why).
	Versions   int    `json:"versions"`
	Uploads    int    `json:"uploads"`
	Resealed   int    `json:"resealed"`
	Failed     int    `json:"failed"`
	FirstError string `json:"firstError,omitempty"`
}

// KMSStatus implements AdminClient.
func (c *client) KMSStatus(ctx context.Context) (*KMSStatus, error) {
	var out KMSStatus
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/kms/status"}, &out)
}

// ListKMSKeys implements AdminClient.
func (c *client) ListKMSKeys(ctx context.Context, prefix string) ([]KMSKey, error) {
	var q url.Values
	if prefix != "" {
		q = url.Values{"prefix": {prefix}}
	}
	var out struct {
		Items []KMSKey `json:"items"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/kms/keys", query: q}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []KMSKey{}
	}
	return out.Items, nil
}

// CreateKMSKey implements AdminClient.
func (c *client) CreateKMSKey(ctx context.Context, id string) (*KMSKey, error) {
	var out KMSKey
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/kms/keys", body: struct {
		ID string `json:"id"`
	}{id}}, &out)
}

// ResealKMSKeys implements AdminClient.
func (c *client) ResealKMSKeys(ctx context.Context, bucket string) (*KMSResealResult, error) {
	var q url.Values
	if bucket != "" {
		q = url.Values{"bucket": {bucket}}
	}
	var out KMSResealResult
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/kms/reseal", query: q}, &out)
}
