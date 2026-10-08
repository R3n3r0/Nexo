package rpc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/vmihailenco/msgpack/v5"
)

// Streams are sequences of frames:
//
//	type (1 byte) | length (4 bytes, big endian) | payload | CRC-32C of payload (4 bytes)
//
// A data stream is data frames closed by an end frame; a message stream is
// msgpack messages closed by an end frame. An error frame (a msgpack
// wireError) ends a stream that failed half-way. A stream that ends without
// an end or error frame was cut: readers report io.ErrUnexpectedEOF.
const (
	frameData  byte = 1
	frameMsg   byte = 2
	frameEnd   byte = 3
	frameError byte = 4

	frameHeaderSize = 5
	frameTrailer    = 4
	// maxFramePayload bounds the payload a reader accepts.
	maxFramePayload = 8 << 20
	// dataChunk is the payload size of the data frames a writer emits.
	dataChunk = 128 << 10
)

// Content types of the internode calls.
const (
	contentTypeMsgpack = "application/x-msgpack"
	contentTypeStream  = "application/x-nexo-stream"
)

// frameWriter writes frames to a buffered writer.
type frameWriter struct {
	w     *bufio.Writer
	table *crc32.Table
	enc   *msgpack.Encoder
	buf   bytes.Buffer
	hdr   [frameHeaderSize]byte
	sum   [frameTrailer]byte
}

func newFrameWriter(w io.Writer) *frameWriter {
	fw := &frameWriter{w: bufio.NewWriterSize(w, dataChunk+frameHeaderSize+frameTrailer), table: crc32.MakeTable(crc32.Castagnoli)}
	fw.enc = msgpack.NewEncoder(&fw.buf)
	return fw
}

func (fw *frameWriter) frame(typ byte, payload []byte) error {
	fw.hdr[0] = typ
	binary.BigEndian.PutUint32(fw.hdr[1:], uint32(len(payload)))
	binary.BigEndian.PutUint32(fw.sum[:], crc32.Checksum(payload, fw.table))
	if _, err := fw.w.Write(fw.hdr[:]); err != nil {
		return err
	}
	if _, err := fw.w.Write(payload); err != nil {
		return err
	}
	_, err := fw.w.Write(fw.sum[:])
	return err
}

// Data writes p as data frames.
func (fw *frameWriter) Data(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), dataChunk)
		if err := fw.frame(frameData, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// Msg writes v as a message frame.
func (fw *frameWriter) Msg(v any) error {
	fw.buf.Reset()
	if err := fw.enc.Encode(v); err != nil {
		return fmt.Errorf("rpc: encode %T: %w", v, err)
	}
	return fw.frame(frameMsg, fw.buf.Bytes())
}

// End closes the stream successfully.
func (fw *frameWriter) End() error { return fw.frame(frameEnd, nil) }

// Error closes the stream with err.
func (fw *frameWriter) Error(err error) error {
	we, _ := encodeError(err)
	fw.buf.Reset()
	if eerr := fw.enc.Encode(we); eerr != nil {
		return eerr
	}
	return fw.frame(frameError, fw.buf.Bytes())
}

// CopyData streams r as data frames until io.EOF and returns the number of
// bytes copied. Read errors are returned as such.
func (fw *frameWriter) CopyData(r io.Reader) (int64, error) {
	buf := make([]byte, dataChunk)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			total += int64(n)
			if werr := fw.Data(buf[:n]); werr != nil {
				return total, werr
			}
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// Flush writes the buffered frames.
func (fw *frameWriter) Flush() error { return fw.w.Flush() }

// Buffered returns the number of bytes waiting for a flush.
func (fw *frameWriter) Buffered() int { return fw.w.Buffered() }

// frameReader reads frames, verifying their checksums. It is an io.Reader
// over the data frames of a data stream and decodes the messages of a
// message stream.
type frameReader struct {
	r     *bufio.Reader
	node  string
	table *crc32.Table
	hdr   [frameHeaderSize]byte
	sum   [frameTrailer]byte
	buf   []byte
	// data is the unread part of the current data frame.
	data []byte
	// err is the sticky end of the stream (io.EOF, remote error, ...).
	err error
}

func newFrameReader(r io.Reader, node string) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(r, 64<<10), node: node, table: crc32.MakeTable(crc32.Castagnoli)}
}

// next reads the next frame. A stream cut before an end frame is
// io.ErrUnexpectedEOF; an error frame is decoded into the remote error.
func (fr *frameReader) next() (byte, []byte, error) {
	if fr.err != nil {
		return 0, nil, fr.err
	}
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return 0, nil, fr.fail(cut(err))
	}
	typ, n := fr.hdr[0], binary.BigEndian.Uint32(fr.hdr[1:])
	if n > maxFramePayload {
		return 0, nil, fr.fail(fmt.Errorf("%w: frame of %d bytes from %s", ErrProtocol, n, fr.node))
	}
	if cap(fr.buf) < int(n) {
		fr.buf = make([]byte, n)
	}
	payload := fr.buf[:n]
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		return 0, nil, fr.fail(cut(err))
	}
	if _, err := io.ReadFull(fr.r, fr.sum[:]); err != nil {
		return 0, nil, fr.fail(cut(err))
	}
	if crc32.Checksum(payload, fr.table) != binary.BigEndian.Uint32(fr.sum[:]) {
		return 0, nil, fr.fail(fmt.Errorf("%w: frame checksum mismatch from %s", ErrProtocol, fr.node))
	}
	switch typ {
	case frameEnd:
		return 0, nil, fr.fail(io.EOF)
	case frameError:
		var we wireError
		if err := msgpack.Unmarshal(payload, &we); err != nil {
			return 0, nil, fr.fail(fmt.Errorf("%w: undecodable error frame from %s: %v", ErrProtocol, fr.node, err))
		}
		return 0, nil, fr.fail(decodeError(fr.node, we))
	case frameData, frameMsg:
		return typ, payload, nil
	default:
		return 0, nil, fr.fail(fmt.Errorf("%w: unknown frame type %d from %s", ErrProtocol, typ, fr.node))
	}
}

func (fr *frameReader) fail(err error) error {
	fr.err = err
	return err
}

// cut maps the end of the underlying stream before an end frame.
func cut(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Read implements io.Reader over the data frames.
func (fr *frameReader) Read(p []byte) (int, error) {
	for len(fr.data) == 0 {
		typ, payload, err := fr.next()
		if err != nil {
			return 0, err
		}
		if typ != frameData {
			return 0, fr.fail(fmt.Errorf("%w: message frame in a data stream from %s", ErrProtocol, fr.node))
		}
		fr.data = payload
	}
	n := copy(p, fr.data)
	fr.data = fr.data[n:]
	return n, nil
}

// Next decodes the next message frame into v; io.EOF ends the stream.
func (fr *frameReader) Next(v any) error {
	typ, payload, err := fr.next()
	if err != nil {
		return err
	}
	if typ != frameMsg {
		return fr.fail(fmt.Errorf("%w: data frame in a message stream from %s", ErrProtocol, fr.node))
	}
	if err := msgpack.Unmarshal(payload, v); err != nil {
		return fr.fail(fmt.Errorf("%w: decode message from %s: %v", ErrProtocol, fr.node, err))
	}
	return nil
}
