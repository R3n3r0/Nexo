package rpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// roundTrip sends err over the wire.
func roundTrip(t *testing.T, err error) error {
	t.Helper()
	we, _ := encodeError(err)
	raw, merr := msgpack.Marshal(we)
	require.NoError(t, merr)
	var back wireError
	require.NoError(t, msgpack.Unmarshal(raw, &back))
	return decodeError("node-1:9000", back)
}

// TestErrorCodes: every sentinel survives the wire, wrapped or not, and
// the codes are unique.
func TestErrorCodes(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range errorCodes() {
		t.Run(c.code, func(t *testing.T) {
			require.False(t, seen[c.code], "duplicate code")
			seen[c.code] = true
			got := roundTrip(t, fmt.Errorf("storage: write b/k: %w", c.err))
			assert.ErrorIs(t, got, c.err)
			assert.Contains(t, got.Error(), "node-1:9000")
			assert.Contains(t, got.Error(), "storage: write b/k")
		})
	}
}

func TestErrorEncoding(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		is     []error
		status int
	}{
		{name: "specific sentinel keeps its identity", err: fmt.Errorf("read: %w", storage.ErrFileCorrupt),
			is: []error{storage.ErrFileCorrupt, domain.ErrFaultyDisk}, status: 500},
		{name: "every sentinel of the chain", err: fmt.Errorf("%w: conflicts: %w", domain.ErrInvalidObjectName, domain.ErrObjectNotFound),
			is: []error{domain.ErrInvalidObjectName, domain.ErrObjectNotFound}, status: 400},
		{name: "joined errors", err: errors.Join(domain.ErrDiskNotFound, domain.ErrNodeOffline),
			is: []error{domain.ErrDiskNotFound, domain.ErrNodeOffline}, status: 503},
		{name: "context", err: fmt.Errorf("walk: %w", context.Canceled), is: []error{context.Canceled}, status: 499},
		{name: "short stream", err: fmt.Errorf("create: %w", io.ErrUnexpectedEOF), is: []error{io.ErrUnexpectedEOF}, status: 400},
		{name: "rpc sentinels", err: fmt.Errorf("x: %w", ErrDeploymentMismatch), is: []error{ErrDeploymentMismatch, domain.ErrAccessDenied}, status: 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, status := encodeError(tt.err)
			assert.Equal(t, tt.status, status)
			got := roundTrip(t, tt.err)
			for _, want := range tt.is {
				assert.ErrorIs(t, got, want)
			}
			assert.NotErrorIs(t, got, domain.ErrBucketNotFound)
		})
	}

	// S3 decoration.
	s3err := domain.NewS3Error(domain.ErrInvalidObjectName, "XNexoParentIsObject", "a parent is an object").WithResource("b/k")
	got := roundTrip(t, fmt.Errorf("commit: %w", s3err))
	var back *domain.S3Error
	require.ErrorAs(t, got, &back)
	assert.Equal(t, "XNexoParentIsObject", back.Code)
	assert.Equal(t, "a parent is an object", back.Message)
	assert.Equal(t, "b/k", back.Resource)
	assert.ErrorIs(t, got, domain.ErrInvalidObjectName)

	// An error without sentinel keeps its text.
	got = roundTrip(t, errors.New("something odd"))
	assert.Contains(t, got.Error(), "something odd")
	assert.Contains(t, got.Error(), codeInternal)
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	fw := newFrameWriter(&buf)
	require.NoError(t, fw.Data(bytes.Repeat([]byte("x"), dataChunk+10)))
	require.NoError(t, fw.End())
	require.NoError(t, fw.Flush())
	stream := buf.Bytes()

	data, err := io.ReadAll(newFrameReader(bytes.NewReader(stream), "n"))
	require.NoError(t, err)
	assert.Len(t, data, dataChunk+10)

	// A flipped bit is detected by the checksum.
	corrupt := append([]byte(nil), stream...)
	corrupt[frameHeaderSize+100] ^= 1
	_, err = io.ReadAll(newFrameReader(bytes.NewReader(corrupt), "n"))
	assert.ErrorIs(t, err, ErrProtocol)
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	// A stream cut before its end frame.
	_, err = io.ReadAll(newFrameReader(bytes.NewReader(stream[:len(stream)-frameHeaderSize-frameTrailer]), "n"))
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	_, err = io.ReadAll(newFrameReader(bytes.NewReader(stream[:10]), "n"))
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)

	// An error frame ends the stream with the remote error.
	buf.Reset()
	fw = newFrameWriter(&buf)
	require.NoError(t, fw.Data([]byte("partial")))
	require.NoError(t, fw.Error(fmt.Errorf("read part: %w", domain.ErrFaultyDisk)))
	require.NoError(t, fw.Flush())
	data, err = io.ReadAll(newFrameReader(bytes.NewReader(buf.Bytes()), "n"))
	assert.Equal(t, "partial", string(data))
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	// Message streams.
	buf.Reset()
	fw = newFrameWriter(&buf)
	require.NoError(t, fw.Msg(storage.WalkEntry{Name: "a/", IsDir: true}))
	require.NoError(t, fw.Msg(storage.WalkEntry{Name: "a/b", Metadata: []byte{1, 2}}))
	require.NoError(t, fw.End())
	require.NoError(t, fw.Flush())
	fr := newFrameReader(bytes.NewReader(buf.Bytes()), "n")
	var e1, e2 storage.WalkEntry
	require.NoError(t, fr.Next(&e1))
	require.NoError(t, fr.Next(&e2))
	assert.Equal(t, storage.WalkEntry{Name: "a/", IsDir: true}, e1)
	assert.Equal(t, []byte{1, 2}, e2.Metadata)
	assert.ErrorIs(t, fr.Next(&e1), io.EOF)
	_, err = newFrameReader(bytes.NewReader(buf.Bytes()), "n").Read(make([]byte, 4))
	assert.ErrorIs(t, err, ErrProtocol, "a message in a data stream")

	// Oversized and unknown frames.
	_, err = newFrameReader(bytes.NewReader([]byte{frameData, 0xff, 0xff, 0xff, 0xff}), "n").Read(make([]byte, 1))
	assert.ErrorIs(t, err, ErrProtocol)
	buf.Reset()
	fw = newFrameWriter(&buf)
	require.NoError(t, fw.frame(9, []byte("?")))
	require.NoError(t, fw.Flush())
	_, err = newFrameReader(bytes.NewReader(buf.Bytes()), "n").Read(make([]byte, 1))
	assert.ErrorIs(t, err, ErrProtocol)
}

func TestVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v1.2.3", "v1.9.0", true},
		{"1.2.3", "v1.0.0", true},
		{"v1.2.3", "v2.0.0", false},
		{"dev", "v2.0.0", true},
		{"v3.0.0", "abc123", true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, compatibleVersions(tt.a, tt.b), "%s vs %s", tt.a, tt.b)
	}
}

func TestParentPrefix(t *testing.T) {
	for in, want := range map[string]string{"x": "", "a/b": "a/", "a/b/c": "a/b/", "a/": "", "a/b/": "a/"} {
		assert.Equal(t, want, parentPrefix(in), in)
	}
}
