// Package audit records who did what: one Entry per API call, delivered to
// the configured sinks (structured log in M0, webhook/Kafka targets later).
package audit

import (
	"context"
	"time"
)

// Service records audit entries.
type Service interface {
	// Log records one entry. It must not block the request path for long;
	// implementations buffer or drop when the sink is unavailable.
	Log(ctx context.Context, e Entry) error
}

// Entry is one audit record, modeled on the MinIO audit log format.
type Entry struct {
	Version      string            `json:"version"`
	DeploymentID string            `json:"deploymentId,omitempty"`
	Time         time.Time         `json:"time"`
	RequestID    string            `json:"requestId,omitempty"`
	API          API               `json:"api"`
	RemoteHost   string            `json:"remoteHost,omitempty"`
	UserAgent    string            `json:"userAgent,omitempty"`
	AccessKey    string            `json:"accessKey,omitempty"`
	ParentUser   string            `json:"parentUser,omitempty"`
	ReqQuery     map[string]string `json:"requestQuery,omitempty"`
	ReqHeader    map[string]string `json:"requestHeader,omitempty"`
	RespHeader   map[string]string `json:"responseHeader,omitempty"`
	Tags         map[string]any    `json:"tags,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// API describes the operation performed.
type API struct {
	Name            string        `json:"name"`
	Bucket          string        `json:"bucket,omitempty"`
	Object          string        `json:"object,omitempty"`
	Status          string        `json:"status,omitempty"`
	StatusCode      int           `json:"statusCode,omitempty"`
	InputBytes      int64         `json:"rx"`
	OutputBytes     int64         `json:"tx"`
	TimeToFirstByte time.Duration `json:"timeToFirstByte,omitempty"`
	TimeToResponse  time.Duration `json:"timeToResponse,omitempty"`
}

// EntryVersion is the current Entry format version.
const EntryVersion = "1"
