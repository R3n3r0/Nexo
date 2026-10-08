package s3

import "context"

type operationKey struct{}

type targetKey struct{}

// target is the resource addressed by a request, as resolved by the
// Dispatcher.
type target struct {
	scope  Scope
	bucket string
	key    string
	// virtualHost reports a "{bucket}.{domain}" request.
	virtualHost bool
}

func withOperation(ctx context.Context, op Operation) context.Context {
	return context.WithValue(ctx, operationKey{}, op)
}

// OperationFromContext returns the operation selected by the Dispatcher.
func OperationFromContext(ctx context.Context) (Operation, bool) {
	op, ok := ctx.Value(operationKey{}).(Operation)
	return op, ok
}

func withTarget(ctx context.Context, t target) context.Context {
	return context.WithValue(ctx, targetKey{}, t)
}

func targetFromContext(ctx context.Context) (target, bool) {
	t, ok := ctx.Value(targetKey{}).(target)
	return t, ok
}
