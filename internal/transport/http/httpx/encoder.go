package httpx

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Content types.
const (
	ContentTypeXML     = "application/xml"
	ContentTypeJSON    = "application/json"
	ContentTypeProblem = "application/problem+json"
)

// Encoder writes responses.
type Encoder interface {
	// WriteXML encodes v as XML with the S3 declaration header.
	WriteXML(w http.ResponseWriter, status int, v any)
	// WriteJSON encodes v as JSON.
	WriteJSON(w http.ResponseWriter, status int, v any)
	// WriteProblem writes an RFC 9457 problem document, filling Instance
	// and RequestID from the request.
	WriteProblem(w http.ResponseWriter, r *http.Request, p Problem)
	// WriteProblemError maps err to a Problem and writes it.
	WriteProblemError(w http.ResponseWriter, r *http.Request, err error)
	// WriteS3Error maps err to an S3 error document (XML) with the request
	// ids. NotModified and HEAD requests get an empty body.
	WriteS3Error(w http.ResponseWriter, r *http.Request, err error)
}

// Decoder reads request bodies.
type Decoder interface {
	// DecodeXML reads at most maxBytes from r into v; failures return
	// domain.ErrMalformedXML.
	DecodeXML(r io.Reader, maxBytes int64, v any) error
	// DecodeJSON reads at most maxBytes from r into v; failures return
	// domain.ErrInvalidArgument.
	DecodeJSON(r io.Reader, maxBytes int64, v any) error
}

// Codec is both an Encoder and a Decoder.
type Codec interface {
	Encoder
	Decoder
}

// Option configures the codec.
type Option func(*codec)

// WithIndent pretty-prints JSON and XML (debugging).
func WithIndent() Option {
	return func(c *codec) { c.indent = true }
}

type codec struct {
	indent bool
}

var _ Codec = (*codec)(nil)

// New returns the default codec.
func New(opts ...Option) Codec {
	c := &codec{}
	for _, o := range opts {
		o(c)
	}
	return c
}

// S3ErrorResponse is the XML body of an S3 error.
type S3ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Key       string   `xml:"Key,omitempty"`
	Bucket    string   `xml:"BucketName,omitempty"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}

// WriteXML implements Encoder.
func (c *codec) WriteXML(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if c.indent {
		enc.Indent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ContentTypeXML)
	w.Header().Set("Content-Length", fmt.Sprint(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// WriteJSON implements Encoder.
func (c *codec) WriteJSON(w http.ResponseWriter, status int, v any) {
	c.writeJSON(w, status, ContentTypeJSON, v)
}

func (c *codec) writeJSON(w http.ResponseWriter, status int, contentType string, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if c.indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// WriteProblem implements Encoder.
func (c *codec) WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if r != nil {
		if p.Instance == "" {
			p.Instance = r.URL.Path
		}
		if p.RequestID == "" {
			p.RequestID = RequestIDFrom(r.Context())
		}
	}
	c.writeJSON(w, p.Status, ContentTypeProblem, p)
}

// WriteProblemError implements Encoder.
func (c *codec) WriteProblemError(w http.ResponseWriter, r *http.Request, err error) {
	c.WriteProblem(w, r, ProblemFromError(err))
}

// WriteS3Error implements Encoder.
func (c *codec) WriteS3Error(w http.ResponseWriter, r *http.Request, err error) {
	code, message := MapS3Error(err)
	var requestID, hostID, resource string
	if r != nil {
		requestID = RequestIDFrom(r.Context())
		hostID = HostIDFrom(r.Context())
		resource = r.URL.Path
	}
	if res := S3ErrorResource(err); res != "" {
		resource = res
	}
	if r != nil && (code.HTTPStatus == http.StatusNotModified || r.Method == http.MethodHead) {
		w.WriteHeader(code.HTTPStatus)
		return
	}
	c.WriteXML(w, code.HTTPStatus, S3ErrorResponse{
		Code:      code.Code,
		Message:   message,
		Resource:  resource,
		RequestID: requestID,
		HostID:    hostID,
	})
}

// DecodeXML implements Decoder.
func (c *codec) DecodeXML(r io.Reader, maxBytes int64, v any) error {
	body, err := readLimited(r, maxBytes)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrMalformedXML, err)
	}
	if err := xml.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrMalformedXML, err)
	}
	return nil
}

// DecodeJSON implements Decoder.
func (c *codec) DecodeJSON(r io.Reader, maxBytes int64, v any) error {
	body, err := readLimited(r, maxBytes)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrInvalidArgument, err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: invalid JSON body: %v", domain.ErrInvalidArgument, err)
	}
	return nil
}

func readLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", maxBytes)
	}
	return body, nil
}
