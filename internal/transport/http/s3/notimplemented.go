package s3

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// notImplemented returns a handler answering NotImplemented for op.
func notImplemented(enc httpx.Encoder, op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enc.WriteS3Error(w, r, fmt.Errorf("%s: %w", op, domain.ErrNotImplemented))
	}
}

// mountNotImplemented registers every operation with a NotImplemented handler.
func mountNotImplemented(d Dispatcher, enc httpx.Encoder, ops []Operation) {
	for _, op := range ops {
		d.Handle(op, notImplemented(enc, op.Name))
	}
}

// mountAll registers the handlers of a group in a deterministic order (by
// operation name). When available is false every operation answers
// NotImplemented: the group advertises its routes without its services.
func mountAll(d Dispatcher, enc httpx.Encoder, ops map[Operation]http.HandlerFunc, available bool) {
	names := make([]Operation, 0, len(ops))
	for op := range ops {
		names = append(names, op)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Name < names[j].Name })
	for _, op := range names {
		fn := ops[op]
		if !available {
			fn = notImplemented(enc, op.Name)
		}
		d.Handle(op, fn)
	}
}

// registerStandalone mounts the group on a private dispatcher registered on r.
func registerStandalone(r chi.Router, enc httpx.Encoder, g Group) {
	d := NewDispatcher(enc)
	g.Mount(d)
	d.Register(r)
}
