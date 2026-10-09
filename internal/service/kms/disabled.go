package kms

import (
	"context"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// disabled is the KMS of a deployment without one.
type disabled struct{}

var _ Service = (*disabled)(nil)

// NewDisabled returns the KMS of a deployment without KMS: Status reports
// it as not configured and every key operation fails with
// domain.ErrKMSNotConfigured (SSE-S3 and SSE-KMS are refused, SSE-C does
// not need a KMS).
func NewDisabled() Service { return disabled{} }

func errNotConfigured() error {
	return fmt.Errorf("kms: %w", domain.ErrKMSNotConfigured)
}

// Status implements Service.
func (disabled) Status(context.Context) (Status, error) { return Status{}, nil }

// DefaultKeyID implements Service.
func (disabled) DefaultKeyID() string { return "" }

// CreateKey implements Service.
func (disabled) CreateKey(context.Context, string) error { return errNotConfigured() }

// ListKeys implements Service.
func (disabled) ListKeys(context.Context, string) ([]KeyInfo, error) {
	return nil, errNotConfigured()
}

// GenerateKey implements Service.
func (disabled) GenerateKey(context.Context, string, Context) (DEK, error) {
	return DEK{}, errNotConfigured()
}

// Decrypt implements Service.
func (disabled) Decrypt(context.Context, string, []byte, Context) ([]byte, error) {
	return nil, errNotConfigured()
}

// ReEncrypt implements Service.
func (disabled) ReEncrypt(context.Context, string, []byte, Context) ([]byte, error) {
	return nil, errNotConfigured()
}
