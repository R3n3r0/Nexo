package httpx

import (
	"errors"
	"io"
	"os"
)

// Zero-copy downloads (docs/bench, T-9.2). A body that is the bytes of a
// file as they are on the drive (the parts of a single-drive version with
// neither encryption nor compression) reaches the response writer as a
// file section: an *io.LimitedReader over an *os.File positioned at the
// first byte. Every writer of the chain hands it on through io.ReaderFrom
// down to the http.ResponseWriter of the server, whose connection gives it
// to sendfile(2) over HTTP/1.1 without TLS: the kernel copies the page
// cache to the socket, no read into user space. Over TLS or HTTP/2, or
// for any other reader, the bytes are copied through Write as before.

// filePiece is the most handed to the ReadFrom of the response writer at
// once: each piece is one write of the stall bounds (SendGuard and the
// server's stall guard), armed before it. Larger pieces mean fewer
// sendfile calls and deadline updates; a client that sends less than
// filePiece within the timeout keeps its download as long as it receives
// minFileProgress bytes per timeout (see CopyFileSection).
const filePiece = 256 << 10

// minFileProgress is the progress that resumes a piece whose deadline
// expired: the size of one write of a buffered copy (io.Copy), so that a
// slow client is held to the same rate as with plain writes.
const minFileProgress = 32 << 10

// FileSection returns src as a file section (see above).
func FileSection(src io.Reader) (*io.LimitedReader, bool) {
	lr, ok := src.(*io.LimitedReader)
	if !ok {
		return nil, false
	}
	_, ok = lr.R.(*os.File)
	return lr, ok
}

// CopyFileSection copies the file section lr to w piece by piece (at most
// filePiece bytes each, through the ReadFrom of w when it has one: the
// sendfile of the server; the writers of the response chain all have one),
// calling arm before each piece. A piece that fails on its write deadline
// after sending at least minFileProgress bytes is resumed under a new
// deadline: the client is slow, not stalled. A file shorter than the
// section ends the copy early with no error, like a reader reaching EOF.
func CopyFileSection(w io.Writer, lr *io.LimitedReader, arm func()) (int64, error) {
	var total int64
	for lr.N > 0 {
		arm()
		piece := &io.LimitedReader{R: lr.R, N: min(lr.N, filePiece)}
		n, err := io.Copy(w, piece)
		lr.N -= n
		total += n
		if err != nil {
			if n >= minFileProgress && errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return total, err
		}
		if piece.N > 0 {
			// EOF before the end of the section.
			break
		}
	}
	return total, nil
}

// writerOnly hides the io.ReaderFrom of a writer from io.Copy.
type writerOnly struct{ io.Writer }

// ReadFrom implements io.ReaderFrom: a file section goes to the response
// by pieces, each within the timeout (CopyFileSection); any other reader is
// copied through Write, each write within the timeout, as io.Copy does.
func (g *SendGuard) ReadFrom(src io.Reader) (int64, error) {
	lr, ok := FileSection(src)
	if !ok {
		return io.Copy(writerOnly{g}, src)
	}
	return CopyFileSection(g.w, lr, g.arm)
}
