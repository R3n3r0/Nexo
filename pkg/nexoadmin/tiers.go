package nexoadmin

import (
	"context"
	"net/http"
	"time"
)

// TierInput are the settings of a remote tier of the lifecycle
// transitions (S-STO-12). A replacement (EditTier) takes the credentials
// and, when set, the region and the storage class only.
type TierInput struct {
	// Name is the storage class the transitions name (A-Z, 0-9, '-',
	// '_').
	Name string `json:"name,omitempty"`
	// Endpoint is the URL of the S3 API, Bucket the remote bucket, Prefix
	// the prefix of the objects in it (ending with '/').
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Region   string `json:"region,omitempty"`
	// StorageClass is the storage class of the remote objects.
	StorageClass string `json:"storageClass,omitempty"`
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
}

// Tier is a remote tier, without its secret.
type Tier struct {
	Name         string    `json:"name"`
	Endpoint     string    `json:"endpoint"`
	Bucket       string    `json:"bucket"`
	Prefix       string    `json:"prefix,omitempty"`
	Region       string    `json:"region,omitempty"`
	StorageClass string    `json:"storageClass,omitempty"`
	AccessKey    string    `json:"accessKey"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

type tierList struct {
	Tiers []Tier `json:"tiers"`
}

// ListTiers implements AdminClient.
func (c *client) ListTiers(ctx context.Context) ([]Tier, error) {
	var out tierList
	if err := c.do(ctx, request{method: http.MethodGet, path: "/tiers"}, &out); err != nil {
		return nil, err
	}
	return out.Tiers, nil
}

// AddTier implements AdminClient.
func (c *client) AddTier(ctx context.Context, in TierInput) (*Tier, error) {
	var out Tier
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/tiers", body: in}, &out)
}

// EditTier implements AdminClient.
func (c *client) EditTier(ctx context.Context, name string, in TierInput) (*Tier, error) {
	var out Tier
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("tiers", name), body: in}, &out)
}

// RemoveTier implements AdminClient.
func (c *client) RemoveTier(ctx context.Context, name string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("tiers", name)}, nil)
}
