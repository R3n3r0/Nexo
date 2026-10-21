package s3

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/s3select"
)

// SelectObjectContent (S-OBJ-16): the request is an XML document, the
// response an AWS event stream of Records, Progress, Stats and End events
// (or an error event once the stream has started).

// SelectObjectContentRequest is the body of SelectObjectContent.
type SelectObjectContentRequest struct {
	XMLName         xml.Name `xml:"SelectObjectContentRequest"`
	Expression      string   `xml:"Expression"`
	ExpressionType  string   `xml:"ExpressionType"`
	RequestProgress *struct {
		Enabled bool `xml:"Enabled"`
	} `xml:"RequestProgress"`
	InputSerialization struct {
		CompressionType string `xml:"CompressionType"`
		CSV             *struct {
			AllowQuotedRecordDelimiter bool   `xml:"AllowQuotedRecordDelimiter"`
			Comments                   string `xml:"Comments"`
			FieldDelimiter             string `xml:"FieldDelimiter"`
			FileHeaderInfo             string `xml:"FileHeaderInfo"`
			QuoteCharacter             string `xml:"QuoteCharacter"`
			QuoteEscapeCharacter       string `xml:"QuoteEscapeCharacter"`
			RecordDelimiter            string `xml:"RecordDelimiter"`
		} `xml:"CSV"`
		JSON *struct {
			Type string `xml:"Type"`
		} `xml:"JSON"`
		Parquet *struct{} `xml:"Parquet"`
	} `xml:"InputSerialization"`
	OutputSerialization struct {
		CSV *struct {
			QuoteFields          string `xml:"QuoteFields"`
			QuoteEscapeCharacter string `xml:"QuoteEscapeCharacter"`
			RecordDelimiter      string `xml:"RecordDelimiter"`
			FieldDelimiter       string `xml:"FieldDelimiter"`
			QuoteCharacter       string `xml:"QuoteCharacter"`
		} `xml:"CSV"`
		JSON *struct {
			RecordDelimiter string `xml:"RecordDelimiter"`
		} `xml:"JSON"`
	} `xml:"OutputSerialization"`
	ScanRange *struct {
		Start *int64 `xml:"Start"`
		End   *int64 `xml:"End"`
	} `xml:"ScanRange"`
}

// selectRequest converts the XML request.
func (x *SelectObjectContentRequest) selectRequest() s3select.Request {
	req := s3select.Request{Expression: x.Expression, ExpressionType: x.ExpressionType}
	if x.RequestProgress != nil {
		req.RequestProgress = x.RequestProgress.Enabled
	}
	in := x.InputSerialization
	req.Input.CompressionType = in.CompressionType
	if c := in.CSV; c != nil {
		req.Input.CSV = &s3select.CSVInput{
			FileHeaderInfo: c.FileHeaderInfo, Comments: c.Comments, QuoteEscapeCharacter: c.QuoteEscapeCharacter,
			RecordDelimiter: c.RecordDelimiter, FieldDelimiter: c.FieldDelimiter, QuoteCharacter: c.QuoteCharacter,
			AllowQuotedRecordDelimiter: c.AllowQuotedRecordDelimiter,
		}
	}
	if j := in.JSON; j != nil {
		req.Input.JSON = &s3select.JSONInput{Type: j.Type}
	}
	req.Input.Parquet = in.Parquet != nil
	out := x.OutputSerialization
	if c := out.CSV; c != nil {
		req.Output.CSV = &s3select.CSVOutput{
			QuoteFields: c.QuoteFields, QuoteEscapeCharacter: c.QuoteEscapeCharacter, RecordDelimiter: c.RecordDelimiter,
			FieldDelimiter: c.FieldDelimiter, QuoteCharacter: c.QuoteCharacter,
		}
	}
	if j := out.JSON; j != nil {
		req.Output.JSON = &s3select.JSONOutput{RecordDelimiter: j.RecordDelimiter}
	}
	if sr := x.ScanRange; sr != nil {
		req.ScanRange = &s3select.ScanRange{Start: sr.Start, End: sr.End}
	}
	return req
}

// selectObjectContent serves SelectObjectContent: it needs s3:GetObject,
// as S3 does.
func (h *objectRoutes) selectObjectContent(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	if h.deps.Select == nil {
		h.fail(w, r, notImplemented("SelectObjectContent is not implemented"))
		return
	}
	if t := r.URL.Query().Get("select-type"); t != "2" {
		h.fail(w, r, invalidArgument("select-type must be 2"))
		return
	}
	var doc SelectObjectContentRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &doc); err != nil {
		h.fail(w, r, err)
		return
	}
	req := doc.selectRequest()
	if err := h.deps.Select.Validate(req); err != nil {
		h.fail(w, r, err)
		return
	}
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	opts := object.GetObjectOptions{SSECustomerKey: ckey}
	rd, err := h.deps.Objects.GetObject(r.Context(), bucketName, key, opts)
	if err != nil {
		h.writeReadError(w, r, err, bucketName, key, opts)
		return
	}
	defer func() { _ = rd.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	es := &eventStream{w: w}
	stats, err := h.deps.Select.Run(r.Context(), req, rd, es)
	if err != nil {
		if es.err != nil {
			// The client is gone: nothing more can be written.
			return
		}
		code, msg := "InternalError", "We encountered an internal error. Please try again."
		var s3err *domain.S3Error
		if errors.As(err, &s3err) && s3err.Code != "" {
			code, msg = s3err.Code, s3err.Message
		} else {
			h.deps.Logger.ErrorContext(r.Context(), "s3: select failed", "bucket", bucketName, "key", key, "err", err)
		}
		es.errorEvent(code, msg)
		return
	}
	if es.statsEvent("Stats", stats) == nil {
		_ = es.event("End", "", nil)
	}
}

// eventStream writes the messages of an AWS event stream; it is the sink
// of a select run.
type eventStream struct {
	w   http.ResponseWriter
	err error
}

var _ s3select.Sink = (*eventStream)(nil)

// Records implements s3select.Sink.
func (es *eventStream) Records(p []byte) error {
	return es.event("Records", "application/octet-stream", p)
}

// Progress implements s3select.Sink.
func (es *eventStream) Progress(s s3select.Stats) error {
	return es.statsEvent("Progress", s)
}

func (es *eventStream) statsEvent(name string, s s3select.Stats) error {
	payload := fmt.Sprintf("<%s><BytesScanned>%d</BytesScanned><BytesProcessed>%d</BytesProcessed><BytesReturned>%d</BytesReturned></%s>",
		name, s.BytesScanned, s.BytesProcessed, s.BytesReturned, name)
	return es.event(name, "text/xml", []byte(payload))
}

func (es *eventStream) event(eventType, contentType string, payload []byte) error {
	headers := [][2]string{{":event-type", eventType}}
	if contentType != "" {
		headers = append(headers, [2]string{":content-type", contentType})
	}
	headers = append(headers, [2]string{":message-type", "event"})
	return es.write(headers, payload)
}

func (es *eventStream) errorEvent(code, message string) {
	_ = es.write([][2]string{{":error-code", code}, {":error-message", message}, {":message-type", "error"}}, nil)
}

// write frames a message: total length, headers length, prelude CRC, the
// headers (name, type 7 = string, value), the payload and the CRC of the
// whole message.
func (es *eventStream) write(headers [][2]string, payload []byte) error {
	if es.err != nil {
		return es.err
	}
	var hb bytes.Buffer
	for _, h := range headers {
		hb.WriteByte(byte(len(h[0])))
		hb.WriteString(h[0])
		hb.WriteByte(7)
		_ = binary.Write(&hb, binary.BigEndian, uint16(len(h[1]))) // #nosec G115 -- short header values
		hb.WriteString(h[1])
	}
	total := 12 + hb.Len() + len(payload) + 4
	msg := make([]byte, 0, total)
	msg = binary.BigEndian.AppendUint32(msg, uint32(total))    // #nosec G115 -- a chunk of at most a few MiB
	msg = binary.BigEndian.AppendUint32(msg, uint32(hb.Len())) // #nosec G115 -- a few short headers
	msg = binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg[:8]))
	msg = append(msg, hb.Bytes()...)
	msg = append(msg, payload...)
	msg = binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
	if _, err := es.w.Write(msg); err != nil {
		es.err = err
		return err
	}
	if f, ok := es.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
