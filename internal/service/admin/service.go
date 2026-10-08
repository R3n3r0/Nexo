// Package admin is the facade used by the Admin API and the CLI for
// server-wide operations: information, data usage, healing and service
// control.
//
//go:generate mockery --name Service --output ./mock --outpkg mock --with-expecter
package admin

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Service exposes server administration.
type Service interface {
	// ServerInfo returns the server/cluster information.
	ServerInfo(ctx context.Context) (*domain.ServerInfo, error)
	// DataUsage returns the data usage per bucket.
	DataUsage(ctx context.Context) (*DataUsage, error)
	// Heal starts (or queries) a healing operation.
	Heal(ctx context.Context, in HealInput) (*HealResult, error)
	// ServiceAction restarts or stops the server.
	ServiceAction(ctx context.Context, action Action) error
}

// DataUsage is the data usage summary.
type DataUsage struct {
	LastUpdate    time.Time              `json:"lastUpdate"`
	ObjectsCount  uint64                 `json:"objectsCount"`
	VersionsCount uint64                 `json:"versionsCount"`
	TotalSize     uint64                 `json:"totalSize"`
	BucketsCount  uint64                 `json:"bucketsCount"`
	Buckets       map[string]BucketUsage `json:"buckets"`
}

// BucketUsage is the data usage of one bucket.
type BucketUsage struct {
	Size          uint64 `json:"size"`
	ObjectsCount  uint64 `json:"objectsCount"`
	VersionsCount uint64 `json:"versionsCount"`
	DeleteMarkers uint64 `json:"deleteMarkers"`
}

// HealInput are the parameters of Heal.
type HealInput struct {
	Bucket    string
	Prefix    string
	Recursive bool
	DryRun    bool
	// ID queries an existing healing operation instead of starting one.
	ID string
}

// HealResult is the state of a healing operation.
type HealResult struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"startedAt"`
	Finished  bool      `json:"finished"`
	Scanned   uint64    `json:"scanned"`
	Healed    uint64    `json:"healed"`
	Failed    uint64    `json:"failed"`
}

// Action is a service control action.
type Action string

// Service actions.
const (
	ActionRestart Action = "restart"
	ActionStop    Action = "stop"
)

// ActionHandler performs service actions; app provides it.
type ActionHandler interface {
	Handle(ctx context.Context, action Action) error
}

// ActionHandlerFunc adapts a function to ActionHandler.
type ActionHandlerFunc func(ctx context.Context, action Action) error

// Handle implements ActionHandler.
func (f ActionHandlerFunc) Handle(ctx context.Context, action Action) error { return f(ctx, action) }
