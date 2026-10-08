package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/vmihailenco/msgpack/v5"
)

// The handlers of the route groups only decode the call, invoke a service
// and encode the answer (docs/12 §12.3); these adapters do the decoding and
// encoding for the three shapes of call.

// maxUnaryBody bounds the body of a unary call (metadata with inline data,
// small documents).
const maxUnaryBody = 64 << 20

// unary adapts fn to a unary call: msgpack In in, msgpack Out out.
func unary[In, Out any](fn func(ctx context.Context, in In) (Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in In
		if err := decodeBody(w, r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := fn(r.Context(), in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeReply(w, out)
	}
}

// upload adapts fn to a call whose body is a frame stream: the arguments
// message then the data, which fn reads from body.
func upload[In, Out any](fn func(ctx context.Context, in In, body io.Reader) (Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fr := newFrameReader(r.Body, "caller")
		var in In
		if err := fr.Next(&in); err != nil {
			writeError(w, fmt.Errorf("%w: upload arguments: %w", ErrProtocol, err))
			return
		}
		out, err := fn(r.Context(), in, fr)
		if err != nil {
			writeError(w, err)
			return
		}
		writeReply(w, out)
	}
}

// download adapts fn to a call answered by a data stream.
func download[In any](fn func(ctx context.Context, in In) (io.ReadCloser, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in In
		if err := decodeBody(w, r, &in); err != nil {
			writeError(w, err)
			return
		}
		rc, err := fn(r.Context(), in)
		if err != nil {
			writeError(w, err)
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set("Content-Type", contentTypeStream)
		w.WriteHeader(http.StatusOK)
		fw := newFrameWriter(w)
		if _, err := fw.CopyData(rc); err != nil {
			_ = fw.Error(err)
		} else {
			_ = fw.End()
		}
		_ = fw.Flush()
	}
}

// streamFlushSize is the amount of buffered messages that triggers a flush,
// so that the caller starts consuming a long stream early.
const streamFlushSize = 16 << 10

// stream adapts fn to a call answered by a message stream: fn emits the
// messages. An error before the first message is a plain error answer; a
// later one closes the stream with an error frame.
func stream[In, Item any](fn func(ctx context.Context, in In, emit func(Item) error) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in In
		if err := decodeBody(w, r, &in); err != nil {
			writeError(w, err)
			return
		}
		rc := http.NewResponseController(w)
		var fw *frameWriter
		emitted := 0
		err := fn(r.Context(), in, func(item Item) error {
			if fw == nil {
				w.Header().Set("Content-Type", contentTypeStream)
				w.WriteHeader(http.StatusOK)
				fw = newFrameWriter(w)
			}
			if err := fw.Msg(item); err != nil {
				return err
			}
			emitted++
			if emitted == 1 || fw.Buffered() >= streamFlushSize {
				if err := fw.Flush(); err != nil {
					return err
				}
				_ = rc.Flush()
			}
			return nil
		})
		if fw == nil {
			if err != nil {
				writeError(w, err)
				return
			}
			w.Header().Set("Content-Type", contentTypeStream)
			w.WriteHeader(http.StatusOK)
			fw = newFrameWriter(w)
		}
		if err != nil {
			_ = fw.Error(err)
		} else {
			_ = fw.End()
		}
		_ = fw.Flush()
	}
}

// decodeBody decodes the msgpack body of a unary call into v; an empty
// body leaves v zero.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUnaryBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("%w: call body over %d bytes", ErrProtocol, tooLarge.Limit)
		}
		return fmt.Errorf("%w: read call body: %v", ErrProtocol, err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := msgpack.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: decode %T: %v", ErrProtocol, v, err)
	}
	return nil
}

// writeReply writes a successful unary answer.
func writeReply(w http.ResponseWriter, v any) {
	data, err := msgpack.Marshal(v)
	if err != nil {
		writeError(w, fmt.Errorf("rpc: encode %T: %w", v, err))
		return
	}
	w.Header().Set("Content-Type", contentTypeMsgpack)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// writeError writes an error answer: the wire form of err.
func writeError(w http.ResponseWriter, err error) {
	we, status := encodeError(err)
	data, merr := msgpack.Marshal(we)
	if merr != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeMsgpack)
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
