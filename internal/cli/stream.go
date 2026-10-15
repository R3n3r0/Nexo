package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"time"
)

// DefaultReconnectDelay is the first delay before a live stream (admin
// trace, admin logs) that ended or failed is opened again; it doubles
// after every failed attempt up to MaxReconnectDelay.
const DefaultReconnectDelay = time.Second

// MaxReconnectDelay bounds the delay between two attempts to open a live
// stream again.
const MaxReconnectDelay = 30 * time.Second

// FollowOptions configure Follow.
type FollowOptions struct {
	// ReconnectDelay is the first delay before the stream is opened again
	// (0: DefaultReconnectDelay).
	ReconnectDelay time.Duration
	// Max stops after that many items (0: no limit).
	Max int
	// OnReconnect is called before waiting delay to open again the stream
	// that ended or failed with err (io.EOF: the server ended it).
	OnReconnect func(err error, delay time.Duration)
	// OnDropped is called with the items the server dropped since the
	// previous call, because the client did not keep up.
	OnDropped func(dropped uint64)
}

// Follow passes the items of a live stream to each until ctx is done or
// opts.Max items were passed (nil is returned), or each fails (its error
// is returned). The stream is opened with open; when it ends or fails with
// a transient error (the server restarted, the network dropped, a 5xx) it
// is opened again after a growing delay. The failure of the first open
// and the permanent ones (access denied, invalid filter) are returned.
func Follow[T any](ctx context.Context, open func(context.Context) (EventStream[T], error), each func(*T) error, opts FollowOptions) error {
	base := opts.ReconnectDelay
	if base <= 0 {
		base = DefaultReconnectDelay
	}
	delay := base
	count := 0
	first := true
	for {
		stream, err := open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if first || !IsTransientStreamError(err) {
				return err
			}
		} else {
			first = false
			delay = base
			done, readErr := drain(ctx, stream, each, opts, &count)
			if done || ctx.Err() != nil {
				return readErr
			}
			if !IsTransientStreamError(readErr) {
				return readErr
			}
			err = readErr
		}
		if opts.OnReconnect != nil {
			opts.OnReconnect(err, delay)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		delay = min(2*delay, MaxReconnectDelay)
	}
}

// drain reads stream until it ends or fails (the error is returned with
// done false) or Follow must stop (done true, with the error of each).
func drain[T any](ctx context.Context, stream EventStream[T], each func(*T) error, opts FollowOptions, count *int) (done bool, err error) {
	defer func() { _ = stream.Close() }()
	var dropped uint64
	report := func() {
		if n := stream.Dropped(); n > dropped {
			if opts.OnDropped != nil {
				opts.OnDropped(n - dropped)
			}
			dropped = n
		}
	}
	for {
		item, err := stream.Next()
		report()
		if err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			return false, err
		}
		if item == nil {
			continue
		}
		if err := each(item); err != nil {
			return true, err
		}
		*count++
		if opts.Max > 0 && *count >= opts.Max {
			return true, nil
		}
	}
}

// IsTransientStreamError reports whether a live stream that failed with
// err may work when opened again: the end of the stream, a network error
// or a server error (5xx, 429).
func IsTransientStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var adminErr *AdminError
	if errors.As(err, &adminErr) {
		return adminErr.API.Status >= 500 || adminErr.API.Status == 429
	}
	var netErr net.Error
	var urlErr *url.Error
	return errors.As(err, &netErr) || errors.As(err, &urlErr)
}
