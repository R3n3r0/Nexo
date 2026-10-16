package httpx

import (
	"context"
	"sync"
)

// OperationHolder carries the name of the API operation selected by a
// router from an outer middleware to the handler. Middlewares install it in
// the context before routing; handlers fill it with SetOperationName.
type OperationHolder struct {
	mu   sync.Mutex
	name string
	// bucket and object are the target the handler resolved (target
	// reports whether one was recorded).
	bucket, object string
	target         bool
	// err is the error a handler answered with (SetOperationError).
	err error
	// throttled is the kind of the rate limit that refused the request
	// (SetOperationThrottled).
	throttled string
}

type operationKey struct{}

// WithOperationHolder returns a context carrying a fresh holder.
func WithOperationHolder(ctx context.Context) (context.Context, *OperationHolder) {
	h := &OperationHolder{}
	return context.WithValue(ctx, operationKey{}, h), h
}

// SetOperationName records the operation name when a holder is present.
func SetOperationName(ctx context.Context, name string) {
	if h, ok := ctx.Value(operationKey{}).(*OperationHolder); ok {
		h.mu.Lock()
		h.name = name
		h.mu.Unlock()
	}
}

// Name returns the recorded operation name ("" when none).
func (h *OperationHolder) Name() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.name
}

// OperationNameFrom returns the operation name recorded in the context.
func OperationNameFrom(ctx context.Context) string {
	h, _ := ctx.Value(operationKey{}).(*OperationHolder)
	return h.Name()
}

// SetOperationTarget records the bucket and object a handler resolved for
// the request (the bucket of a custom domain, of a virtual host or of the
// path), so that the observers of the request (audit, metrics, traces)
// report what was actually read or written, when a holder is present.
func SetOperationTarget(ctx context.Context, bucket, object string) {
	if h, ok := ctx.Value(operationKey{}).(*OperationHolder); ok {
		h.mu.Lock()
		h.bucket, h.object, h.target = bucket, object, true
		h.mu.Unlock()
	}
}

// Target returns the recorded bucket and object; ok is false when none
// was recorded.
func (h *OperationHolder) Target() (bucket, object string, ok bool) {
	if h == nil {
		return "", "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bucket, h.object, h.target
}

// OperationTargetFrom returns the target recorded in the context.
func OperationTargetFrom(ctx context.Context) (bucket, object string, ok bool) {
	h, _ := ctx.Value(operationKey{}).(*OperationHolder)
	return h.Target()
}

// SetOperationError records the error a handler answered with, when a
// holder is present: the responses carry a curated message, the logs of
// the request (middleware.NewLogging) its whole chain with the request id.
func SetOperationError(ctx context.Context, err error) {
	if h, ok := ctx.Value(operationKey{}).(*OperationHolder); ok && err != nil {
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
	}
}

// Err returns the recorded error (nil when none).
func (h *OperationHolder) Err() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// SetOperationThrottled records that a rate limit refused the request
// (its kind: ip, user, bucket, distribution, concurrency), when a holder is
// present: the audit log and the live trace mark the request.
func SetOperationThrottled(ctx context.Context, kind string) {
	if h, ok := ctx.Value(operationKey{}).(*OperationHolder); ok {
		h.mu.Lock()
		h.throttled = kind
		h.mu.Unlock()
	}
}

// Throttled returns the kind of the rate limit that refused the request
// ("" when none did).
func (h *OperationHolder) Throttled() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.throttled
}
