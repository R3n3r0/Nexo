// Package event implements bucket notifications: S3-format events published
// by the object layer and dispatched asynchronously to configured targets
// (webhook in M0; Kafka, NATS, AMQP, Redis, PostgreSQL later). The event
// model (Event, Name, Filter matching) lives in event.go; this file holds
// the service contracts only.
package event

import (
	"context"

	"github.com/r3n3r0/nexo/internal/lifecycle"
)

// Service publishes events and manages the notification targets.
type Service interface {
	// Publish enqueues an event for delivery to every target whose filter
	// matches. It returns an error only when the event cannot be queued.
	Publish(ctx context.Context, e Event) error

	// SetTarget creates or replaces a target from its configuration.
	SetTarget(ctx context.Context, cfg TargetConfig) error
	GetTarget(ctx context.Context, id string) (TargetConfig, error)
	ListTargets(ctx context.Context) ([]TargetConfig, error)
	RemoveTarget(ctx context.Context, id string) error
}

// Dispatcher is a Service with a delivery goroutine managed by lifecycle.
type Dispatcher interface {
	Service
	lifecycle.Runner
}

// Target delivers events to one destination.
type Target interface {
	// ID returns the target identifier (pure accessor, no context).
	ID() string
	Send(ctx context.Context, e Event) error
	Close(ctx context.Context) error
}

// TargetFactory builds a Target from its configuration.
type TargetFactory interface {
	New(ctx context.Context, cfg TargetConfig) (Target, error)
}

// Target types.
const (
	TypeWebhook = "webhook"
)

// TargetConfig is the configuration of one target.
type TargetConfig struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Endpoint is the destination URL (webhook).
	Endpoint string `json:"endpoint"`
	// AuthToken is sent as a Bearer token when set.
	AuthToken string `json:"authToken,omitempty"`
	Enabled   bool   `json:"enabled"`
	Filter    Filter `json:"filter"`
}

// Filter selects the events a target receives. Empty fields match all.
type Filter struct {
	// Events are the event name patterns (e.g. "s3:ObjectCreated:*").
	Events []Name `json:"events,omitempty"`
	Bucket string `json:"bucket,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
}
