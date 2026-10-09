package rpc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// transportFunc adapts a function to http.RoundTripper.
type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// lateEOF is a request body whose end is read late: once after bytes
// went through, its reads wait for the answer of the node to be handed to
// the caller (the transport writer lagging behind on a loaded machine).
type lateEOF struct {
	io.ReadCloser
	after    int64
	answered <-chan struct{}
	n        int64
}

func (l *lateEOF) Read(p []byte) (int, error) {
	if l.n >= l.after {
		<-l.answered
		time.Sleep(20 * time.Millisecond)
	}
	n, err := l.ReadCloser.Read(p)
	l.n += int64(n)
	return n, err
}

// countingBody counts the bytes read from a request body.
type countingBody struct {
	io.ReadCloser
	n atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// onClose runs fn when the body is closed.
type onClose struct {
	io.ReadCloser
	fn func()
}

func (o *onClose) Close() error {
	err := o.ReadCloser.Close()
	o.fn()
	return err
}

// slowAnswers delays the body of every answer after its headers.
type slowAnswers struct {
	next  http.Handler
	delay time.Duration
}

func (s slowAnswers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.next.ServeHTTP(&lateBodyWriter{ResponseWriter: w, delay: s.delay}, r)
}

// lateBodyWriter sends the headers of an answer, then waits before its body.
type lateBodyWriter struct {
	http.ResponseWriter
	delay   time.Duration
	written bool
}

func (l *lateBodyWriter) Write(p []byte) (int, error) {
	if !l.written {
		l.written = true
		if f, ok := l.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(l.delay)
	}
	return l.ResponseWriter.Write(p)
}

// remoteDriveWith is remoteDrive with connection options; with a positive
// answerDelay the node sends the body of its answers that long after their
// headers.
func remoteDriveWith(t testing.TB, dir string, answerDelay time.Duration, opts ...rpc.ConnOption) (storage.StorageAPI, rpc.Conn) {
	t.Helper()
	local := openLocal(t, dir)
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}})
	require.NoError(t, err)
	ln := listen(t)
	addr := ln.Addr().String()
	srv, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, addr), Routes: []rpc.Routes{routes}, Logger: discard()})
	require.NoError(t, err)
	var handler http.Handler = srv
	if answerDelay > 0 {
		handler = slowAnswers{next: srv, delay: answerDelay}
	}
	serve(t, ln, handler)
	conn := dial(t, newAuth(t, "client:9000"), addr, opts...)
	client, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Logger: discard()},
		domain.Endpoint{Scheme: "http", Host: addr, Path: dir}, rpc.WithDiskCheckInterval(50*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, conn
}

// TestUploadAnswerBeforeBodyEnd: the node answers an upload (headers
// first, body a moment later) before the transport has read the end of
// the request body. The answer must be read in full before the request
// body is closed: closing it first aborted the stream ("io: read/write on
// closed pipe") and the healthy node was reported offline (T-6.1).
func TestUploadAnswerBeforeBodyEnd(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	// The first upload measures the size of the framed request body; the
	// next ones hold the read of its end until the answer is handed over.
	var (
		delayed atomic.Int32
		framed  atomic.Int64
	)
	wrap := func(next http.RoundTripper) http.RoundTripper {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Body == nil || r.Header.Get("Content-Type") != "application/x-nexo-stream" {
				return next.RoundTrip(r)
			}
			size := framed.Load()
			if size == 0 {
				counter := &countingBody{ReadCloser: r.Body}
				r.Body = counter
				resp, err := next.RoundTrip(r)
				if err == nil {
					resp.Body = &onClose{ReadCloser: resp.Body, fn: func() { framed.Store(counter.n.Load()) }}
				}
				return resp, err
			}
			delayed.Add(1)
			answered := make(chan struct{})
			r.Body = &lateEOF{ReadCloser: r.Body, after: size, answered: answered}
			resp, err := next.RoundTrip(r)
			close(answered)
			return resp, err
		})
	}
	d, conn := remoteDriveWith(t, t.TempDir()+"/remote", 50*time.Millisecond, rpc.WithTransportWrapper(wrap))
	for i := range 4 {
		path := storage.TmpDir + "/" + uuid.NewString() + "/part.1"
		require.NoError(t, d.CreateFile(ctx, domain.SystemBucket, path, int64(len(payload)), bytes.NewReader(payload)), "upload %d", i)
		require.True(t, conn.IsOnline())
	}
	assert.Positive(t, delayed.Load(), "the uploads went through the delaying body")
}

// TestSequentialUploadsStress: many uploads to a healthy node over
// loopback, a few at a time, all succeed and never open the circuit.
func TestSequentialUploadsStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	ctx := context.Background()
	d, conn := remoteDrive(t, t.TempDir()+"/remote")
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	var (
		wg       sync.WaitGroup
		failures atomic.Int32
		first    atomic.Value
	)
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				path := storage.TmpDir + "/" + uuid.NewString() + "/part.1"
				if err := d.CreateFile(ctx, domain.SystemBucket, path, int64(len(payload)), bytes.NewReader(payload)); err != nil {
					failures.Add(1)
					first.CompareAndSwap(nil, errors.Join(err, errors.New("worker "+string(rune('0'+w))+" upload "+time.Duration(i).String())))
					continue
				}
				_ = d.Delete(ctx, domain.SystemBucket, path, storage.DeleteOptions{})
			}
		}()
	}
	wg.Wait()
	if n := failures.Load(); n > 0 {
		t.Fatalf("%d uploads to a healthy node failed, first: %v", n, first.Load())
	}
	assert.True(t, conn.IsOnline())
}

// TestBrokenExchangeHealthyNode: an exchange that breaks while the node
// still answers its pings fails, but does not open the circuit.
func TestBrokenExchangeHealthyNode(t *testing.T) {
	ctx := context.Background()
	var breakNext atomic.Bool
	wrap := func(next http.RoundTripper) http.RoundTripper {
		return transportFunc(func(r *http.Request) (*http.Response, error) {
			if breakNext.CompareAndSwap(true, false) {
				return nil, errors.New("injected: stream reset")
			}
			return next.RoundTrip(r)
		})
	}
	d, conn := remoteDriveWith(t, t.TempDir()+"/remote", 0, rpc.WithTransportWrapper(wrap))
	require.NoError(t, d.MakeVol(ctx, "b"))
	breakNext.Store(true)
	_, err := d.StatVol(ctx, "b")
	require.ErrorIs(t, err, domain.ErrNodeOffline, "the broken call fails")
	// The background ping succeeds: the node stays online.
	time.Sleep(200 * time.Millisecond)
	assert.True(t, conn.IsOnline())
	_, err = d.StatVol(ctx, "b")
	require.NoError(t, err)
}
