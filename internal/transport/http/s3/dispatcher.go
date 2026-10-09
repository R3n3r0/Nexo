package s3

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

type opKey struct {
	method string
	scope  Scope
}

type opEntry struct {
	op Operation
	h  http.HandlerFunc
}

// DispatcherDeps are the dependencies of the Dispatcher.
type DispatcherDeps struct {
	// Enc writes the routing errors (default httpx.New()).
	Enc httpx.Encoder
}

// dispatcher is the default Dispatcher: a table of operations indexed by
// method and scope, each resolved by its discriminating query parameter.
type dispatcher struct {
	enc     httpx.Encoder
	domains []string
	// hostBuckets resolves the custom hosts of the buckets (nil: none).
	hostBuckets HostBuckets
	ops         map[opKey][]opEntry
}

var _ Dispatcher = (*dispatcher)(nil)

// NewDispatcher returns an empty Dispatcher. WithDomains enables
// virtual-host style requests.
func NewDispatcher(deps DispatcherDeps, opts ...Option) Dispatcher {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	o := newOptions(opts)
	return &dispatcher{enc: deps.Enc, domains: o.domains, hostBuckets: o.hostBuckets, ops: make(map[opKey][]opEntry)}
}

// Handle implements Dispatcher.
func (d *dispatcher) Handle(op Operation, h http.HandlerFunc) {
	if h == nil {
		panic(fmt.Sprintf("s3: nil handler for %s", op.Name))
	}
	key := opKey{method: op.Method, scope: op.Scope}
	for _, e := range d.ops[key] {
		if e.op.Query == op.Query && e.op.QueryValue == op.QueryValue {
			panic(fmt.Sprintf("s3: operation %s conflicts with %s (%s scope %d query %q)", op.Name, e.op.Name, op.Method, op.Scope, op.Query))
		}
	}
	d.ops[key] = append(d.ops[key], opEntry{op: op, h: h})
}

// Register implements Routes: the dispatcher serves the whole URL space
// ("/" and "/*", every method); more specific routes registered on the
// same router (health, metrics) take precedence, for the methods they
// serve (see APIDeps.System for virtual-host requests). It also installs
// S3-flavored 404/405 handlers on the router.
func (d *dispatcher) Register(r chi.Router) {
	r.Handle("/", http.HandlerFunc(d.serve))
	r.Handle("/*", http.HandlerFunc(d.serve))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		d.enc.WriteS3Error(w, r, fmt.Errorf("%w: no such resource", domain.ErrInvalidArgument))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		d.enc.WriteS3Error(w, r, fmt.Errorf("%w: %s", domain.ErrMethodNotAllowed, r.Method))
	})
}

func (d *dispatcher) serve(w http.ResponseWriter, r *http.Request) {
	t := d.resolve(r)
	entries := d.ops[opKey{method: r.Method, scope: t.scope}]
	if len(entries) == 0 {
		d.enc.WriteS3Error(w, r, fmt.Errorf("%w: %s", domain.ErrMethodNotAllowed, r.Method))
		return
	}
	e := match(entries, r.URL.Query())
	if e == nil {
		d.enc.WriteS3Error(w, r, fmt.Errorf("%w: unsupported operation", domain.ErrNotImplemented))
		return
	}
	httpx.SetOperationName(r.Context(), e.op.Name)
	if t.scope == ScopeObject && len(t.key) > domain.MaxObjectKeyLength {
		d.enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidObjectName, "KeyTooLongError", "Your key is too long."))
		return
	}
	ctx := withTarget(withOperation(r.Context(), e.op), t)
	e.h(w, r.WithContext(ctx))
}

// resolve splits the request into scope, bucket and key: virtual-host when
// the Host is "{bucket}.{domain}" for a configured domain, path-style
// otherwise. The key is the decoded path (r.URL.Path).
func (d *dispatcher) resolve(r *http.Request) target {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if bucket, ok := d.hostBucket(r); ok {
		if path == "" {
			return target{scope: ScopeBucket, bucket: bucket, virtualHost: true}
		}
		return target{scope: ScopeObject, bucket: bucket, key: path, virtualHost: true}
	}
	if path == "" {
		return target{scope: ScopeService}
	}
	bucket, key, _ := strings.Cut(path, "/")
	if key == "" {
		return target{scope: ScopeBucket, bucket: bucket}
	}
	return target{scope: ScopeObject, bucket: bucket, key: key}
}

// hostBucket returns the bucket a request addresses by host: a custom
// host of a bucket (HostBuckets), then "{bucket}.{domain}".
func (d *dispatcher) hostBucket(r *http.Request) (string, bool) {
	if d.hostBuckets != nil {
		if bucket, ok := d.hostBuckets(middleware.HostOf(r)); ok {
			return bucket, true
		}
	}
	return virtualHostBucket(d.domains, r.Host)
}

// virtualHostBucket extracts the bucket of a "{bucket}.{domain}" host for
// one of domains.
func virtualHostBucket(domains []string, host string) (string, bool) {
	if len(domains) == 0 {
		return "", false
	}
	host = normalizeHost(host)
	for _, domainName := range domains {
		if bucket, ok := strings.CutSuffix(host, "."+domainName); ok && bucket != "" {
			return bucket, true
		}
	}
	return "", false
}

// Ranks of an operation match, highest first (see Dispatcher).
const (
	rankNone = iota - 1
	rankDefault
	rankAuthParam
	rankSubResource
	rankQueryValue
)

// match selects the operation of a request among the entries of its method
// and scope; nil when none applies.
func match(entries []opEntry, q url.Values) *opEntry {
	var best *opEntry
	bestRank := rankNone
	for i := range entries {
		if rank := matchRank(entries[i].op, q); rank > bestRank {
			best, bestRank = &entries[i], rank
		}
	}
	return best
}

func matchRank(op Operation, q url.Values) int {
	if op.Query == "" {
		return rankDefault
	}
	values, present := q[op.Query]
	if !present {
		return rankNone
	}
	if op.QueryValue != "" {
		if len(values) == 0 || values[0] != op.QueryValue {
			return rankNone
		}
		return rankQueryValue
	}
	if isAuthParam(op.Query) {
		return rankAuthParam
	}
	return rankSubResource
}

// isAuthParam reports a query parameter of the SigV4 query authentication,
// which never names an S3 sub-resource.
func isAuthParam(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "x-amz-")
}
