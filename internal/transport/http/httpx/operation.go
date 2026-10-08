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
