package s3

import "context"

type operationKey struct{}

func withOperation(ctx context.Context, op Operation) context.Context {
	return context.WithValue(ctx, operationKey{}, op)
}

// OperationFromContext returns the operation selected by the Dispatcher.
func OperationFromContext(ctx context.Context) (Operation, bool) {
	op, ok := ctx.Value(operationKey{}).(Operation)
	return op, ok
}
