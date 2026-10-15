package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/kms"
)

// kmsInstruments are the instruments of the KMS decorator.
type kmsInstruments struct {
	requests Counter
	errors   Counter
	duration Histogram
}

// WrapKMS returns k measured: the requests to the KMS by operation
// (generate, decrypt, reencrypt, create, list, status), their failures
// (a missing key, an invalid name or a ciphertext that does not
// authenticate are the caller's errors and are counted as such too, under
// their operation) and their latency.
func WrapKMS(ctx context.Context, k kms.Service, reg Registry) (kms.Service, error) {
	if k == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the KMS decorator needs a KMS and a registry", domain.ErrInvalidArgument)
	}
	m := &kmsInstruments{}
	var err error
	if m.requests, err = reg.Counter(ctx, Definition{Subsystem: "kms", Name: "requests_total",
		Help: "Requests to the KMS, by operation.", Labels: []string{"op"}}); err != nil {
		return nil, fmt.Errorf("metrics: kms: %w", err)
	}
	if m.errors, err = reg.Counter(ctx, Definition{Subsystem: "kms", Name: "errors_total",
		Help: "Failed requests to the KMS, by operation and reason (unavailable, not_found, decryption, other).", Labels: []string{"op", "reason"}}); err != nil {
		return nil, fmt.Errorf("metrics: kms: %w", err)
	}
	if m.duration, err = reg.Histogram(ctx, Definition{Subsystem: "kms", Name: "request_duration_seconds",
		Help: "Latency of the requests to the KMS, by operation.", Labels: []string{"op"}}); err != nil {
		return nil, fmt.Errorf("metrics: kms: %w", err)
	}
	return &measuredKMS{next: k, m: m}, nil
}

// record records a request.
func (m *kmsInstruments) record(ctx context.Context, op string, start time.Time, err error) {
	m.requests.Add(ctx, 1, op)
	m.duration.Observe(ctx, time.Since(start).Seconds(), op)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	reason := "other"
	switch {
	case errors.Is(err, domain.ErrKMSUnavailable):
		reason = "unavailable"
	case errors.Is(err, domain.ErrKMSKeyNotFound):
		reason = "not_found"
	case errors.Is(err, domain.ErrDecryption):
		reason = "decryption"
	case errors.Is(err, domain.ErrKMSNotConfigured):
		reason = "not_configured"
	}
	m.errors.Add(ctx, 1, op, reason)
}

type measuredKMS struct {
	next kms.Service
	m    *kmsInstruments
}

var _ kms.Service = (*measuredKMS)(nil)

// Status implements kms.Service.
func (k *measuredKMS) Status(ctx context.Context) (kms.Status, error) {
	start := time.Now()
	st, err := k.next.Status(ctx)
	k.m.record(ctx, "status", start, err)
	return st, err
}

// DefaultKeyID implements kms.Service.
func (k *measuredKMS) DefaultKeyID() string { return k.next.DefaultKeyID() }

// CreateKey implements kms.Service.
func (k *measuredKMS) CreateKey(ctx context.Context, keyID string) error {
	start := time.Now()
	err := k.next.CreateKey(ctx, keyID)
	k.m.record(ctx, "create", start, err)
	return err
}

// ListKeys implements kms.Service.
func (k *measuredKMS) ListKeys(ctx context.Context, prefix string) ([]kms.KeyInfo, error) {
	start := time.Now()
	keys, err := k.next.ListKeys(ctx, prefix)
	k.m.record(ctx, "list", start, err)
	return keys, err
}

// GenerateKey implements kms.Service.
func (k *measuredKMS) GenerateKey(ctx context.Context, keyID string, kctx kms.Context) (kms.DEK, error) {
	start := time.Now()
	dek, err := k.next.GenerateKey(ctx, keyID, kctx)
	k.m.record(ctx, "generate", start, err)
	return dek, err
}

// Decrypt implements kms.Service.
func (k *measuredKMS) Decrypt(ctx context.Context, keyID string, ciphertext []byte, kctx kms.Context) ([]byte, error) {
	start := time.Now()
	key, err := k.next.Decrypt(ctx, keyID, ciphertext, kctx)
	k.m.record(ctx, "decrypt", start, err)
	return key, err
}

// ReEncrypt implements kms.Service.
func (k *measuredKMS) ReEncrypt(ctx context.Context, keyID string, ciphertext []byte, kctx kms.Context) ([]byte, error) {
	start := time.Now()
	out, err := k.next.ReEncrypt(ctx, keyID, ciphertext, kctx)
	k.m.record(ctx, "reencrypt", start, err)
	return out, err
}
