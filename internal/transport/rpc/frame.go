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
	// dataChunk is the payload size of the data frames a writer emits:
	// one HTTP/2 DATA frame (the peers accept 1 MiB frames) per frame.
	dataChunk = 512 << 10
	// readBufferSize is the buffer of a frame reader: it holds the
	// headers and small frames; large payloads are read past it, straight
	// into their destination.
	readBufferSize = 64 << 10
)

// Content types of the internode calls.
const (
	contentTypeMsgpack = "application/x-msgpack"
	contentTypeStream  = "application/x-nexo-stream"
)

// frameWriter writes frames: messages through a buffered writer, data
// frames assembled in place (header, payload read straight from the
// source, checksum) and written whole, without copying the payload.
type frameWriter struct {
	raw   io.Writer
	w     *bufio.Writer
	table *crc32.Table
	enc   *msgpack.Encoder
	buf   bytes.Buffer
	hdr   [frameHeaderSize]byte
	sum   [frameTrailer]byte
	// data is the buffer of a data frame, allocated with the first one.
	data []byte
}

func newFrameWriter(w io.Writer) *frameWriter {
	fw := &frameWriter{raw: w, w: bufio.NewWriterSize(w, 32<<10), table: crc32.MakeTable(crc32.Castagnoli)}
	fw.enc = msgpack.NewEncoder(&fw.buf)
	return fw
}

// dataFrame returns the buffer of a data frame: header, dataChunk bytes
// of payload, checksum.
func (fw *frameWriter) dataFrame() []byte {
	if fw.data == nil {
		fw.data = make([]byte, frameHeaderSize+dataChunk+frameTrailer)
	}
	return fw.data
}

// writeDataFrame completes the frame of buf holding n bytes of payload and
// writes it whole, after the buffered messages.
func (fw *frameWriter) writeDataFrame(buf []byte, n int) error {
	if err := fw.w.Flush(); err != nil {
		return err
	}
	buf[0] = frameData
	binary.BigEndian.PutUint32(buf[1:frameHeaderSize], uint32(n)) // #nosec G115 -- a frame payload, bounded by the frame buffers (far below 4 GiB)
	end := frameHeaderSize + n
	binary.BigEndian.PutUint32(buf[end:end+frameTrailer], crc32.Checksum(buf[frameHeaderSize:end], fw.table))
	_, err := fw.raw.Write(buf[:end+frameTrailer])
	return err
}

func (fw *frameWriter) frame(typ byte, payload []byte) error {
	fw.hdr[0] = typ
	binary.BigEndian.PutUint32(fw.hdr[1:], uint32(len(payload))) // #nosec G115 -- a frame payload, bounded by the message limits (far below 4 GiB)
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
	buf := fw.dataFrame()
	for len(p) > 0 {
		n := copy(buf[frameHeaderSize:frameHeaderSize+dataChunk], p)
		if err := fw.writeDataFrame(buf, n); err != nil {
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
// bytes copied: each frame is filled straight from r (up to dataChunk
// bytes, or what r has when it ends). Read errors are returned as such.
func (fw *frameWriter) CopyData(r io.Reader) (int64, error) {
	buf := fw.dataFrame()
	var total int64
	for {
		n, err := fill(r, buf[frameHeaderSize:frameHeaderSize+dataChunk])
		if n > 0 {
			total += int64(n)
			if werr := fw.writeDataFrame(buf, n); werr != nil {
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

// fill reads from r until p is full or r fails: unlike io.ReadFull, only
// io.EOF ends the stream (a reader failing with io.ErrUnexpectedEOF, a
// cut stream, is an error).
func fill(r io.Reader, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := r.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
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
	return &frameReader{r: bufio.NewReaderSize(r, readBufferSize), node: node, table: crc32.MakeTable(crc32.Castagnoli)}
}

// header reads the header of the next frame: its type and payload size.
func (fr *frameReader) header() (byte, int, error) {
	if fr.err != nil {
		return 0, 0, fr.err
	}
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return 0, 0, fr.fail(cut(err))
	}
	typ, n := fr.hdr[0], binary.BigEndian.Uint32(fr.hdr[1:])
	if n > maxFramePayload {
		return 0, 0, fr.fail(fmt.Errorf("%w: frame of %d bytes from %s", ErrProtocol, n, fr.node))
	}
	return typ, int(n), nil
}

// payload reads the payload of the current frame into dst (its size) and
// verifies its checksum.
func (fr *frameReader) payload(dst []byte) error {
	if _, err := io.ReadFull(fr.r, dst); err != nil {
		return fr.fail(cut(err))
	}
	if _, err := io.ReadFull(fr.r, fr.sum[:]); err != nil {
		return fr.fail(cut(err))
	}
	if crc32.Checksum(dst, fr.table) != binary.BigEndian.Uint32(fr.sum[:]) {
		return fr.fail(fmt.Errorf("%w: frame checksum mismatch from %s", ErrProtocol, fr.node))
	}
	return nil
}

// next reads the next frame. A stream cut before an end frame is
// io.ErrUnexpectedEOF; an error frame is decoded into the remote error.
func (fr *frameReader) next() (byte, []byte, error) {
	typ, n, err := fr.header()
	if err != nil {
		return 0, nil, err
	}
	if cap(fr.buf) < n {
		fr.buf = make([]byte, n)
	}
	payload := fr.buf[:n]
	if err := fr.payload(payload); err != nil {
		return 0, nil, err
	}
	return fr.frame(typ, payload)
}

// frame interprets a frame read whole.
func (fr *frameReader) frame(typ byte, payload []byte) (byte, []byte, error) {
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

// Read implements io.Reader over the data frames. A data frame that fits
// in p is read straight into it.
func (fr *frameReader) Read(p []byte) (int, error) {
	for len(fr.data) == 0 {
		typ, n, err := fr.header()
		if err != nil {
			return 0, err
		}
		if typ == frameData && n > 0 && n <= len(p) {
			if err := fr.payload(p[:n]); err != nil {
				return 0, err
			}
			return n, nil
		}
		if cap(fr.buf) < n {
			fr.buf = make([]byte, n)
		}
		payload := fr.buf[:n]
		if err := fr.payload(payload); err != nil {
			return 0, err
		}
		typ, payload, err = fr.frame(typ, payload)
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

// WriteTo implements io.WriterTo over the data frames: io.Copy writes
// every payload to w as read, without an intermediate buffer.
func (fr *frameReader) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for {
		if len(fr.data) > 0 {
			n, err := w.Write(fr.data)
			total += int64(n)
			fr.data = fr.data[n:]
			if err != nil {
				return total, err
			}
			continue
		}
		typ, payload, err := fr.next()
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		if typ != frameData {
			return total, fr.fail(fmt.Errorf("%w: message frame in a data stream from %s", ErrProtocol, fr.node))
		}
		fr.data = payload
	}
}

// NextRaw returns a copy of the payload of the next message frame (its
// msgpack encoding); io.EOF ends the stream.
func (fr *frameReader) NextRaw() ([]byte, error) {
	typ, payload, err := fr.next()
	if err != nil {
		return nil, err
	}
	if typ != frameMsg {
		return nil, fr.fail(fmt.Errorf("%w: data frame in a message stream from %s", ErrProtocol, fr.node))
	}
	return bytes.Clone(payload), nil
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
