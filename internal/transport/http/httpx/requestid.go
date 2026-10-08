package httpx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Response header names.
const (
	HeaderAmzRequestID = "x-amz-request-id"
	HeaderAmzID2       = "x-amz-id-2"
)

// IDGenerator produces request identifiers.
type IDGenerator interface {
	// NewRequestID returns a short unique id for x-amz-request-id.
	NewRequestID() string
	// NewHostID returns the longer id for x-amz-id-2.
	NewHostID() string
}

type randomIDs struct{}

var _ IDGenerator = randomIDs{}

// NewIDGenerator returns a cryptographically random id generator.
func NewIDGenerator() IDGenerator { return randomIDs{} }

// NewRequestID implements IDGenerator: 16 uppercase hex characters.
func (randomIDs) NewRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// NewHostID implements IDGenerator: 48 base64 characters.
func (randomIDs) NewHostID() string {
	var b [36]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

type ctxKey int

const (
	requestIDKey ctxKey = iota
	hostIDKey
)

// WithRequestID stores the request and host ids in the context.
func WithRequestID(ctx context.Context, requestID, hostID string) context.Context {
	ctx = context.WithValue(ctx, requestIDKey, requestID)
	return context.WithValue(ctx, hostIDKey, hostID)
}

// RequestIDFrom returns the request id stored in the context, or "".
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// HostIDFrom returns the host id stored in the context, or "".
func HostIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(hostIDKey).(string)
	return v
}
