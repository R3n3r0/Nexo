// Package logs is the log stream of the server (S-OPS-07, docs/05 T-8.3):
// the slog handler of the process is a tee (Service.Handler) that writes
// the records to the output of the server (JSON or text, with a level and
// a format changed at runtime by the "logger" subsystem of the dynamic
// configuration, Configurator) and keeps them in a ring buffer of recent
// entries that the subscribers (the console, the CLI) receive live, from
// every node of the cluster (Peer). The values of the attributes named
// like a secret (authorization, password, token, signature, credential,
// secret key, SSE-C key, cookie) are redacted, so are the presigned
// signatures of the URLs logged.
package logs

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
)

// Entry is one log record.
type Entry struct {
	Time time.Time
	// Node is the node that logged the record.
	Node string
	// Level is DEBUG, INFO, WARN or ERROR.
	Level   string
	Message string
	// Attrs are the attributes of the record, groups flattened with dots
	// ("http.status"), values as text, secrets redacted.
	Attrs map[string]string
}

// Filter selects the entries of a subscription.
type Filter struct {
	// Nodes restricts the nodes (empty: every node).
	Nodes []string
	// Level is the minimum level (empty: every level the server logs).
	Level string
	// Last replays up to Last recent entries of every node before the live
	// ones (0: none).
	Last int
	// Local subscribes to the entries of this node only (a peer streaming
	// its entries to the node of the subscriber).
	Local bool
}

// Subscription is a live subscription. It ends with the context given to
// Subscribe: Entries is then closed.
type Subscription interface {
	// Entries delivers the entries. It is a pure accessor.
	Entries() <-chan Entry
	// Dropped counts the entries lost because the subscriber did not keep
	// up, on this node and on the peers. It is a pure accessor.
	Dropped() uint64
}

// Delivery is one message of the stream of a peer: an entry, or (Entry
// nil) the count of the entries the peer dropped since its last message.
type Delivery struct {
	Entry   *Entry
	Dropped uint64
}

// Peer is the log stream of another node (internode RPC).
type Peer interface {
	// NodeID returns the "host:port" of the peer. It is a pure accessor.
	NodeID() string
	// Logs streams the entries of the peer selected by f (Local set) to fn
	// until ctx ends, fn fails or the stream breaks. fn is called
	// sequentially.
	Logs(ctx context.Context, f Filter, fn func(Delivery) error) error
}

// Settings are the level and format of the output of the server; an empty
// field keeps the one the server started with.
type Settings struct {
	// Level is debug, info, warn or error.
	Level string
	// Format is json or text.
	Format string
}

// Service is the log stream of the node.
type Service interface {
	// Handler returns the handler of the process logger: it writes to the
	// output and records into the service. It is a pure accessor (no I/O):
	// the documented exception to the context rule.
	Handler() slog.Handler
	// Subscribe returns the entries selected by f: the last ones kept in
	// memory (Filter.Last), then the live ones of this node and, unless
	// f.Local, of the peers (reconnected when their stream breaks) until
	// ctx ends. An invalid filter is domain.ErrInvalidArgument.
	Subscribe(ctx context.Context, f Filter) (Subscription, error)
	// Configure applies the level and format of the output (hot reload);
	// an invalid value is domain.ErrInvalidArgument. The format changes
	// only when the service owns the output (a writer, not a handler, was
	// given to it).
	Configure(ctx context.Context, s Settings) error
}

// Configurator applies the "logger" subsystem of the dynamic
// configuration (level, format) to the log stream at start and whenever
// it changes.
type Configurator interface {
	lifecycle.Runner
}
