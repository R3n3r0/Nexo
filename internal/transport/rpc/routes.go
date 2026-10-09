// Package rpc is the internode transport of a distributed deployment
// (docs/03 §3.4): HTTP/2 on the S3 port under Prefix (cleartext h2c, or TLS
// when the server has certificates), msgpack bodies, binary streams with
// framing and checksums for the data, and short-lived HS256 JWTs signed
// with a key derived from the root credentials and the deployment id.
//
// Every group of routes is a Routes service (docs/12 §12.3): StorageRoutes
// exposes the local drives, LockRoutes the local lock server, PeerRoutes the
// peer service (cache invalidations, server info, clock) and
// BootstrapRoutes the startup configuration. Server mounts them, behind the
// authentication, in front of the S3 API. The clients implement the
// service interfaces over a Conn: NewStorageClient is a storage.StorageAPI
// for a drive of another node, NewLockClient a lock.Server, NewPeerClient a
// cluster.Peer and NewBootstrapClient a cluster.BootstrapPeer.
//
// Errors cross the wire as stable codes of the domain sentinels (never as
// text to parse): a remote domain.ErrObjectNotFound is a local
// domain.ErrObjectNotFound for errors.Is. A node that cannot be reached
// answers domain.ErrNodeOffline; its Conn opens a circuit breaker and
// probes the node in the background until it answers again.
package rpc

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Prefix is the path below which the internode RPC is served.
const Prefix = "/nexo/rpc/v1"

// ProtocolVersion is the major version of the internode protocol: a node
// presenting another one is refused.
const ProtocolVersion = 1

// Routes registers a group of internode routes on the router mounted at
// Prefix (the paths are "/<service>/<method>").
type Routes interface {
	Register(r chi.Router)
}

// StorageRoutes exposes the local drives of the node ("/storage/...").
type StorageRoutes interface{ Routes }

// LockRoutes exposes the in-memory lock server of the node ("/lock/...").
type LockRoutes interface{ Routes }

// PeerRoutes exposes the peer service of the node ("/peer/...").
type PeerRoutes interface{ Routes }

// BootstrapRoutes exposes the startup configuration of the node
// ("/bootstrap/...").
type BootstrapRoutes interface{ Routes }

// Server serves the internode routes under Prefix: it authenticates every
// call (internode JWT, deployment, protocol version) and dispatches it to
// the route groups. Wrap mounts it in front of another handler, the S3 API
// of the same port: requests below Prefix are served by the RPC server,
// the others go to next (it implements middleware.Middleware, to be the
// outermost middleware of the S3 server).
type Server interface {
	http.Handler
	Wrap(next http.Handler) http.Handler
}

// Claims are the verified claims of an internode token.
type Claims struct {
	// NodeID is the caller ("host:port" of its S3 endpoint).
	NodeID string
	// DeploymentID is the deployment of the caller, empty while it has
	// not read its format yet (bootstrap).
	DeploymentID string
	// Unbound reports a caller without deployment id calling a node bound
	// to its deployment: it proved the root credentials, but it may only
	// use the calls of the bootstrap (it has not read the format yet).
	Unbound bool
	// Unverified reports a call to a node that does not know its own
	// deployment yet (no format on its drives): the deployment the caller
	// claims cannot be checked, so it may only use the calls of the
	// bootstrap and write the format (and healing tracker) of the drives.
	Unverified bool
	// Protocol is the internode protocol version of the caller.
	Protocol int
	// Version is the build version of the caller.
	Version   string
	IssuedAt  time.Time
	ExpiresAt time.Time
	// BodyHash is the SHA-256 of the arguments the token is bound to
	// (CallBinding.BodyHash): the route adapters compare it with the
	// arguments they decode.
	BodyHash []byte
}

// CallBinding identifies the call a token is issued for: a token is good
// for that call only, once.
type CallBinding struct {
	// Method and Target are the method and the request target (path and
	// query, as sent on the wire) of the call.
	Method, Target string
	// BodyHash is the SHA-256 of the msgpack arguments of the call: the
	// whole body of a unary call or of a call answered by a stream, the
	// first frame of an upload. Verify compares it when it is set; the
	// route adapters compare the arguments they decode with
	// Claims.BodyHash otherwise.
	BodyHash []byte
}

// Authenticator issues and verifies the internode tokens: HS256 JWTs
// signed with a key derived (HKDF-SHA256) from the root credentials and
// the deployment id, each bound to one call (method, target, hash of the
// arguments) and accepted once, so that a token seen on the wire (http
// endpoints, or a fake endpoint the caller was tricked into calling) is
// good for nothing else.
type Authenticator interface {
	// NodeID returns the identity this node signs with. It is a pure
	// accessor (no I/O).
	NodeID() string
	// Token returns a bearer token for one outgoing call to the node
	// audience ("host:port", its node id): it is only valid on that node,
	// for that call, once, and for a few seconds.
	Token(ctx context.Context, audience string, call CallBinding) (string, error)
	// Verify validates the token of an incoming call: signature (first:
	// nothing the token claims is trusted before), audience (this node),
	// expiry, the call it is bound to, its unique id (a replayed token is
	// refused), protocol and build major versions, and the deployment id
	// when both nodes know it. Failures wrap domain.ErrUnauthenticated,
	// ErrDeploymentMismatch or ErrIncompatibleVersion, and never carry the
	// identity of this node or of its deployment.
	Verify(ctx context.Context, token string, call CallBinding) (Claims, error)
	// SetDeploymentID binds the authenticator to the deployment once the
	// bootstrap has read or written the format: from then on tokens carry
	// it and callers of another deployment are refused.
	SetDeploymentID(ctx context.Context, id string)
}

// Conn is the client side of the RPC server of one node: pooled HTTP/2
// connections, authenticated calls with a timeout, and a circuit breaker
// that fails calls fast while the node is offline and probes it in the
// background (health ping) until it answers again. Transport failures wrap
// domain.ErrNodeOffline; errors answered by the node are its domain
// sentinels.
type Conn interface {
	// Node returns the "host:port" of the node. It is a pure accessor.
	Node() string
	// IsOnline reports whether the circuit is closed. It is a pure
	// accessor.
	IsOnline() bool
	// Call performs a unary call: in is sent msgpack-encoded and the
	// answer decoded into out (nil to discard it).
	Call(ctx context.Context, path string, in, out any, opts ...CallOption) error
	// Upload streams body after args (a framed, checksummed stream) and
	// decodes the answer into out. A read error of body aborts the call
	// with that error.
	Upload(ctx context.Context, path string, args any, body io.Reader, out any, opts ...CallOption) error
	// Download performs a call answered by a framed data stream: the
	// reader verifies every frame and returns io.EOF at the end of the
	// stream, the remote error if the node failed while streaming, and
	// io.ErrUnexpectedEOF if the stream was cut.
	Download(ctx context.Context, path string, in any, opts ...CallOption) (io.ReadCloser, error)
	// Stream performs a call answered by a framed stream of messages.
	Stream(ctx context.Context, path string, in any, opts ...CallOption) (MsgStream, error)
	// Close stops the background probe and closes the idle connections.
	Close(ctx context.Context) error
}

// MsgStream is a stream of msgpack messages answered by a node.
type MsgStream interface {
	// Next decodes the next message into v; io.EOF ends the stream.
	Next(v any) error
	// Close releases the stream; closing before the end cancels the call.
	Close() error
}

// CallOption tunes one call.
type CallOption func(*callOptions)

type callOptions struct {
	timeout time.Duration
	params  [][2]string
}
