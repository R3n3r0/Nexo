package rpc

import (
	"net/http"
	"time"
)

// WithTimeout overrides the call timeout of the Conn for one call (for
// streams it bounds the wait for the answer, not the stream itself).
func WithTimeout(d time.Duration) CallOption {
	return func(o *callOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithParam adds a query parameter to the call (the drive of a storage
// call).
func WithParam(key, value string) CallOption {
	return func(o *callOptions) { o.params = append(o.params, [2]string{key, value}) }
}

// The route groups and clients are built like every service, with
// New(deps, opts...) (docs/12 §12.2). Most of their options have no
// setting yet: the types reserve the place in the constructors.
type (
	// ServerOption configures the RPC server.
	ServerOption func(*serverOptions)
	// StorageRoutesOption configures the storage routes.
	StorageRoutesOption func(*storageRoutesOptions)
	// LockRoutesOption configures the lock routes.
	LockRoutesOption func(*lockRoutesOptions)
	// PeerRoutesOption configures the peer routes.
	PeerRoutesOption func(*peerRoutesOptions)
	// BootstrapRoutesOption configures the bootstrap routes.
	BootstrapRoutesOption func(*bootstrapRoutesOptions)
	// LockClientOption configures the client of a remote lock server.
	LockClientOption func(*lockClientOptions)
	// PeerClientOption configures the client of a peer.
	PeerClientOption func(*peerClientOptions)
	// BootstrapClientOption configures the bootstrap client of a peer.
	BootstrapClientOption func(*bootstrapClientOptions)
)

// WithSkip leaves to the next handler of Wrap the requests skip selects
// although their path is below Prefix: the virtual-host requests of the
// S3 API whose object key starts with the RPC prefix
// (<bucket>.<domain>/nexo/rpc/v1/...).
func WithSkip(skip func(r *http.Request) bool) ServerOption {
	return func(o *serverOptions) { o.skip = skip }
}

// WithClientCertRequired refuses the internode calls that do not come over
// TLS with a client certificate verified by the server (mutual TLS,
// --tls-internode-verify): the listener must request the certificates
// (tls.VerifyClientCertIfGiven, since the S3 clients of the same port
// present none) and verify them with the CAs of the deployment.
func WithClientCertRequired() ServerOption {
	return func(o *serverOptions) { o.clientCert = true }
}

// WithAuthFailureLimit sets how many authentication failures a remote
// address may cause in a burst before its calls are refused unverified
// (default 20), and how fast that allowance refills (default one every
// 3s).
func WithAuthFailureLimit(burst int, refill time.Duration) ServerOption {
	return func(o *serverOptions) {
		if burst > 0 && refill > 0 {
			o.failBurst, o.failRefill = burst, refill
		}
	}
}

type (
	serverOptions struct {
		skip       func(r *http.Request) bool
		clientCert bool
		failBurst  int
		failRefill time.Duration
	}
	storageRoutesOptions   struct{}
	lockRoutesOptions      struct{}
	peerRoutesOptions      struct{}
	bootstrapRoutesOptions struct{}
	lockClientOptions      struct{}
	peerClientOptions      struct{}
	bootstrapClientOptions struct{}
)

// apply runs the options over a zero options value.
func apply[O any, F ~func(*O)](opts []F) O {
	var o O
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}
