package s3

import (
	"fmt"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// notImplemented returns a handler answering NotImplemented for op.
func notImplemented(enc httpx.Encoder, op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enc.WriteS3Error(w, r, fmt.Errorf("%s: %w", op, domain.ErrNotImplemented))
	}
}

// mountAll registers every operation with a NotImplemented handler.
func mountAll(d Dispatcher, enc httpx.Encoder, ops []Operation) {
	for _, op := range ops {
		d.Handle(op, notImplemented(enc, op.Name))
	}
}

// registerStandalone mounts the group on a private dispatcher registered on r.
func registerStandalone(r chiRouter, enc httpx.Encoder, g Group) {
	d := NewDispatcher(enc)
	g.Mount(d)
	d.Register(r)
}
