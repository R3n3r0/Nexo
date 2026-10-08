package iam

// NewRootOnly returns the IAM service with the in-memory store (Deps.Store
// is ignored): root is installed by Bootstrap and every other identity
// lives until the process exits. It is kept for the composition root until
// it wires New with NewFileStore; new code should call New.
func NewRootOnly(deps Deps, opts ...Option) Service {
	deps.Store = NewMemoryStore()
	return New(deps, opts...)
}
