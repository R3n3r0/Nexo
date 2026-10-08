package httpx_test

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func TestWriteS3Error(t *testing.T) {
	c := httpx.New()
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req = req.WithContext(httpx.WithRequestID(req.Context(), "REQ1", "HOST1"))
	rec := httptest.NewRecorder()
	c.WriteS3Error(rec, req, domain.ErrObjectNotFound)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"))
	assert.True(t, strings.HasPrefix(rec.Body.String(), xml.Header))
	var body httpx.S3ErrorResponse
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "NoSuchKey", body.Code)
	assert.Equal(t, "/bucket/key", body.Resource)
	assert.Equal(t, "REQ1", body.RequestID)
	assert.Equal(t, "HOST1", body.HostID)

	// HEAD and 304 have no body.
	head := httptest.NewRequest(http.MethodHead, "/b/k", nil)
	rec = httptest.NewRecorder()
	c.WriteS3Error(rec, head, domain.ErrObjectNotFound)
	assert.Equal(t, 404, rec.Code)
	assert.Empty(t, rec.Body.String())
	rec = httptest.NewRecorder()
	c.WriteS3Error(rec, req, domain.ErrNotModified)
	assert.Equal(t, 304, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestWriteProblem(t *testing.T) {
	c := httpx.New()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
	req = req.WithContext(httpx.WithRequestID(req.Context(), "R2", ""))
	rec := httptest.NewRecorder()
	c.WriteProblemError(rec, req, domain.ErrUserAlreadyExists)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"))
	var p httpx.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	assert.Equal(t, "user_already_exists", p.Code)
	assert.Equal(t, "/api/v1/users", p.Instance)
	assert.Equal(t, "R2", p.RequestID)
	assert.Equal(t, "about:blank", p.Type)

	rec = httptest.NewRecorder()
	c.WriteProblem(rec, req, httpx.Problem{Code: "custom"})
	assert.Equal(t, 500, rec.Code)
}

func TestWriteJSONAndXML(t *testing.T) {
	c := httpx.New(httpx.WithIndent())
	rec := httptest.NewRecorder()
	c.WriteJSON(rec, http.StatusCreated, map[string]int{"a": 1})
	assert.Equal(t, 201, rec.Code)
	assert.Equal(t, httpx.ContentTypeJSON, rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"a":1}`, rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get("Content-Length"))

	type doc struct {
		XMLName xml.Name `xml:"Doc"`
		Name    string   `xml:"Name"`
	}
	rec = httptest.NewRecorder()
	c.WriteXML(rec, http.StatusOK, doc{Name: "x"})
	assert.Contains(t, rec.Body.String(), "<Doc>")
	assert.Contains(t, rec.Body.String(), "<Name>x</Name>")
}

func TestDecoders(t *testing.T) {
	c := httpx.New()
	type doc struct {
		XMLName xml.Name `xml:"Doc"`
		Name    string   `xml:"Name"`
	}
	var d doc
	require.NoError(t, c.DecodeXML(strings.NewReader("<Doc><Name>x</Name></Doc>"), 1024, &d))
	assert.Equal(t, "x", d.Name)
	assert.ErrorIs(t, c.DecodeXML(strings.NewReader("<Doc>"), 1024, &d), domain.ErrMalformedXML)
	assert.ErrorIs(t, c.DecodeXML(strings.NewReader(strings.Repeat("a", 20)), 10, &d), domain.ErrMalformedXML)

	var m struct{ A int }
	require.NoError(t, c.DecodeJSON(strings.NewReader(`{"A":2}`), 0, &m))
	assert.Equal(t, 2, m.A)
	assert.ErrorIs(t, c.DecodeJSON(strings.NewReader(`{"B":2}`), 0, &m), domain.ErrInvalidArgument)
	assert.ErrorIs(t, c.DecodeJSON(strings.NewReader(`{`), 0, &m), domain.ErrInvalidArgument)
}

func TestRequestIDsAndHeaders(t *testing.T) {
	g := httpx.NewIDGenerator()
	id1, id2 := g.NewRequestID(), g.NewRequestID()
	assert.Len(t, id1, 16)
	assert.NotEqual(t, id1, id2)
	assert.Len(t, g.NewHostID(), 48)

	h := http.Header{}
	h.Set("X-Amz-Meta-Color", "blue")
	h.Add("x-amz-meta-tags", "a")
	h.Add("x-amz-meta-tags", "b")
	h.Set("X-Amz-Date", "20260101T000000Z")
	h.Set("Content-Type", "text/plain")
	assert.Equal(t, map[string]string{"color": "blue", "tags": "a,b"}, httpx.UserMetadata(h))
	assert.Equal(t, "20260101T000000Z", httpx.AmzHeader(h, httpx.HeaderAmzDate))
	assert.True(t, httpx.HasAmzHeader(h, "x-amz-date"))
	assert.False(t, httpx.HasAmzHeader(h, "x-amz-nope"))
	amz := httpx.AmzHeaders(h)
	assert.Len(t, amz, 3)
	assert.Equal(t, "blue", amz["x-amz-meta-color"])

	out := http.Header{}
	httpx.SetUserMetadata(out, map[string]string{"k": "v"})
	assert.Equal(t, "v", out.Get("x-amz-meta-k"))
}
