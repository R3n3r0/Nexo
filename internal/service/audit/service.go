// Package audit records who did what (S-OPS-06, docs/05 T-8.4): one Entry
// per request served by the S3 API and the Admin API (identity, operation,
// resource, client, outcome, latency, request id, node, sizes), written to
// the structured log of the server, kept in memory for the searches of the
// console (D-MON-03, aggregated over the nodes of a cluster) and delivered
// to the audit targets of the dynamic configuration (audit_webhook,
// audit_kafka), each with the persistent queue of the notification targets
// (internal/service/event).
package audit

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
)

// Service records audit entries.
type Service interface {
	// Log records one entry. It must not block the request path for long;
	// implementations buffer or drop when the sink is unavailable.
	Log(ctx context.Context, e Entry) error
	// Search returns the most recent entries kept in memory that match q,
	// newest first: of this node and, unless q.Local, of the peers. An
	// invalid query is domain.ErrInvalidArgument.
	Search(ctx context.Context, q Query) ([]Entry, error)
}

// Dispatcher is the Service that delivers the entries to the audit
// targets of the configuration, reloaded when they change.
type Dispatcher interface {
	Service
	lifecycle.Runner
}

// Peer is the audit log of another node (internode RPC).
type Peer interface {
	// NodeID returns the "host:port" of the peer. It is a pure accessor.
	NodeID() string
	// SearchAudit returns the entries of the peer matching q (Local set).
	SearchAudit(ctx context.Context, q Query) ([]Entry, error)
}

// Query selects entries.
type Query struct {
	// Nodes restricts the nodes (empty: every node).
	Nodes []string
	// API is the operation (exact, case-insensitive).
	API string
	// Bucket is the bucket of the requests.
	Bucket string
	// User is the access key of the caller or its parent user.
	User string
	// Type is the API that served the requests (s3, admin, web).
	Type string
	// StatusCodes and StatusClasses restrict the statuses (4 for 4xx).
	StatusCodes   []int
	StatusClasses []int
	// ErrorsOnly keeps the failed requests (status >= 400 or an error).
	ErrorsOnly bool
	// Since and Until bound the time of the entries.
	Since time.Time
	Until time.Time
	// RequestID selects the entry of one request.
	RequestID string
	// Limit bounds the entries returned (default 100, at most 1000).
	Limit int
	// Local searches the entries of this node only (the peers ask it).
	Local bool
}

// Entry is one audit record, modeled on the MinIO audit log format.
type Entry struct {
	Version      string `json:"version"`
	DeploymentID string `json:"deploymentId,omitempty"`
	// Node is the node that served the request.
	Node string `json:"node,omitempty"`
	// Type is the API that served the request (s3, admin, web).
	Type       string            `json:"type,omitempty"`
	Time       time.Time         `json:"time"`
	RequestID  string            `json:"requestId,omitempty"`
	API        API               `json:"api"`
	RemoteHost string            `json:"remoteHost,omitempty"`
	UserAgent  string            `json:"userAgent,omitempty"`
	AccessKey  string            `json:"accessKey,omitempty"`
	ParentUser string            `json:"parentUser,omitempty"`
	ReqQuery   map[string]string `json:"requestQuery,omitempty"`
	ReqHeader  map[string]string `json:"requestHeader,omitempty"`
	RespHeader map[string]string `json:"responseHeader,omitempty"`
	Tags       map[string]any    `json:"tags,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// API describes the operation performed.
type API struct {
	Name string `json:"name"`
	// Method and Path are the HTTP method and path of the request.
	Method          string        `json:"method,omitempty"`
	Path            string        `json:"path,omitempty"`
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
