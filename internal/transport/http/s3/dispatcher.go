package s3

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type opKey struct {
	method string
	scope  Scope
}

type opEntry struct {
	op Operation
	h  http.HandlerFunc
}

// dispatcher is the default Dispatcher: a table of operations indexed by
// method and scope, each resolved by its discriminating query parameter.
type dispatcher struct {
	enc httpx.Encoder
	ops map[opKey][]opEntry
}

var _ Dispatcher = (*dispatcher)(nil)

// NewDispatcher returns an empty Dispatcher writing errors with enc.
func NewDispatcher(enc httpx.Encoder) Dispatcher {
	if enc == nil {
		enc = httpx.New()
	}
	return &dispatcher{enc: enc, ops: make(map[opKey][]opEntry)}
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

// Register implements Routes: it mounts the three path-style patterns for
// every HTTP method and installs S3-flavored 404/405 handlers.
func (d *dispatcher) Register(r chi.Router) {
	methods := []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodOptions}
	for _, m := range methods {
		r.Method(m, "/", d.serve(ScopeService))
		r.Method(m, "/{bucket}", d.serve(ScopeBucket))
		r.Method(m, "/{bucket}/", d.serve(ScopeBucket))
		r.Method(m, "/{bucket}/*", d.serve(ScopeObject))
	}
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		d.enc.WriteS3Error(w, r, fmt.Errorf("%w: no such resource", domain.ErrInvalidArgument))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		d.enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, httpx.CodeMethodNotAllowed.Code, httpx.CodeMethodNotAllowed.Description))
	})
}

func (d *dispatcher) serve(scope Scope) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries := d.ops[opKey{method: r.Method, scope: scope}]
		if len(entries) == 0 {
			d.enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, httpx.CodeMethodNotAllowed.Code, httpx.CodeMethodNotAllowed.Description))
			return
		}
		query := r.URL.Query()
		var fallback *opEntry
		for i := range entries {
			e := &entries[i]
			if e.op.Query == "" {
				if fallback == nil {
					fallback = e
				}
				continue
			}
			values, present := query[e.op.Query]
			if !present {
				continue
			}
			if e.op.QueryValue != "" && (len(values) == 0 || values[0] != e.op.QueryValue) {
				continue
			}
			e.h(w, r.WithContext(withOperation(r.Context(), e.op)))
			return
		}
		if fallback != nil {
			fallback.h(w, r.WithContext(withOperation(r.Context(), fallback.op)))
			return
		}
		d.enc.WriteS3Error(w, r, fmt.Errorf("%w: unsupported operation", domain.ErrNotImplemented))
	}
}
