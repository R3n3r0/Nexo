package admin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// maxJSONBody bounds the JSON request bodies of the Admin API.
const maxJSONBody = 1 << 20

// maxPageLimit is the largest page of a list endpoint (and the default).
// Lists answer {items, nextCursor}; the cursor is opaque.
const maxPageLimit = 1000

// Problem codes specific to the Admin API (the others come from
// httpx.Codec.ProblemFromError).
const (
	codeUnauthenticated  = "unauthenticated"
	codeAccessDenied     = "access_denied"
	codeInvalidArgument  = "invalid_argument"
	codeMalformedPolicy  = "malformed_policy"
	codeNotImplemented   = "not_implemented"
	codeInvalidCSRFToken = "invalid_csrf_token"
)

// caller returns the authenticated identity, or the anonymous identity
// when no authentication middleware ran.
func caller(r *http.Request) *domain.Identity {
	if id, ok := middleware.IdentityFromContext(r.Context()); ok {
		return id
	}
	return domain.Anonymous()
}

// requesterOf builds the requester of the authorization service: the
// identity of the request and its condition values.
func requesterOf(r *http.Request) adminsvc.Requester {
	return adminsvc.Requester{Identity: *caller(r), Conditions: requestConditions(r)}
}

// clientIP is the address of the caller: the peer of the connection, or
// the client behind the trusted proxies (middleware.NewForwarded).
func clientIP(r *http.Request) string { return middleware.ClientOf(r).IP }

// requestConditions builds the policy condition context of a request
// (adminsvc.ClientConditions).
func requestConditions(r *http.Request) map[string][]string {
	client := middleware.ClientOf(r)
	return adminsvc.ClientConditions(adminsvc.Client{IP: client.IP, Secure: client.Secure, UserAgent: r.UserAgent(), Referer: r.Referer()})
}

// objectRequestOf describes the S3 request an object route stands for
// (adminsvc.ObjectConditions): the version of the versionId query
// parameter and the x-amz-tagging header, the only x-amz-* header the
// console forwards to the object layer (the tags of an upload).
func objectRequestOf(r *http.Request) adminsvc.ObjectRequest {
	or := adminsvc.ObjectRequest{VersionID: r.URL.Query().Get("versionId")}
	if v := r.Header.Get(adminsvc.HeaderTagging); v != "" {
		or.Headers = map[string]string{adminsvc.HeaderTagging: v}
	}
	return or
}

// nonNil returns s, or an empty slice when s is nil, so that JSON clients
// always receive an array.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// nonNilMap returns m, or an empty map when m is nil, so that JSON clients
// always receive an object.
func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// queryBool parses a boolean query parameter; absent or empty is false,
// anything but a boolean is an invalid argument.
func queryBool(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%w: %s must be a boolean", domain.ErrInvalidArgument, name)
	}
	return b, nil
}

// queryInt parses an optional non-negative integer query parameter.
func queryInt(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %s must be a non-negative integer", domain.ErrInvalidArgument, name)
	}
	return n, nil
}

// optTime returns a pointer to t, nil for the zero time.
func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

// decodeJSON reads a JSON body into v; an empty body is an invalid
// argument unless optional is set.
func decodeJSON(enc httpx.Codec, r *http.Request, v any, optional bool) error {
	if optional && (r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0) {
		return nil
	}
	return enc.DecodeJSON(r.Body, maxJSONBody, v)
}

// readBody reads at most limit bytes of the request body.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", domain.ErrInvalidArgument, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", domain.ErrInvalidArgument, limit)
	}
	return data, nil
}

// --- errors ----------------------------------------------------------------

// PolicyErrorResponse locates an error of a policy document.
type PolicyErrorResponse struct {
	// Statement is 1-based, 0 for document-level errors.
	Statement int    `json:"statement"`
	Sid       string `json:"sid,omitempty"`
	Element   string `json:"element,omitempty"`
	// Line and Column are 1-based, 0 when unknown.
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

func newPolicyErrorResponse(p adminsvc.PolicyProblem) PolicyErrorResponse {
	return PolicyErrorResponse{Statement: p.Statement, Sid: p.Sid, Element: p.Element, Line: p.Line, Column: p.Column, Message: p.Message}
}

// problemDocument is a problem with the extension members of the Admin
// API.
type problemDocument struct {
	httpx.Problem
	// Errors locates the errors of a malformed policy document.
	Errors []PolicyErrorResponse `json:"errors,omitempty"`
}

// writeError maps err to a problem+json response. Policy validation
// errors are reported as malformed_policy with their position.
func writeError(enc httpx.Encoder, w http.ResponseWriter, r *http.Request, err error) {
	if p, ok := adminsvc.ProblemOf(err); ok {
		doc := problemDocument{Problem: enc.ProblemFromError(err), Errors: []PolicyErrorResponse{newPolicyErrorResponse(p)}}
		doc.Status, doc.Code = http.StatusBadRequest, codeMalformedPolicy
		doc.Title = http.StatusText(http.StatusBadRequest)
		writeProblemDocument(w, r, doc)
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		err = fmt.Errorf("%w: request body exceeds %d bytes", domain.ErrEntityTooLarge, tooLarge.Limit)
	}
	enc.WriteProblemError(w, r, err)
}

// writeProblemDocument writes a problem with extension members.
func writeProblemDocument(w http.ResponseWriter, r *http.Request, doc problemDocument) {
	if doc.Type == "" {
		doc.Type = "about:blank"
	}
	if doc.Instance == "" {
		doc.Instance = r.URL.Path
	}
	if doc.RequestID == "" {
		doc.RequestID = httpx.RequestIDFrom(r.Context())
	}
	data, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", httpx.ContentTypeProblem)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(doc.Status)
	_, _ = w.Write(data)
}

// problem writes a problem with an explicit status and code.
func problem(enc httpx.Encoder, w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	enc.WriteProblem(w, r, httpx.Problem{Status: status, Code: code, Detail: detail})
}

// notImplemented answers 501 problem+json.
func notImplemented(enc httpx.Encoder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		problem(enc, w, r, http.StatusNotImplemented, codeNotImplemented, "endpoint not implemented yet")
	}
}

// --- pagination ------------------------------------------------------------

// page are the cursor and limit of a list request.
type page struct {
	// after is the decoded cursor: the sort key of the last item served.
	after string
	limit int
}

// readPage reads the cursor and limit query parameters (limit 1 to 1000,
// default 1000).
func readPage(r *http.Request) (page, error) {
	q := r.URL.Query()
	p := page{limit: maxPageLimit}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			return page{}, fmt.Errorf("%w: limit must be between 1 and %d", domain.ErrInvalidArgument, maxPageLimit)
		}
		p.limit = n
	}
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || len(raw) == 0 {
			return page{}, fmt.Errorf("%w: invalid cursor", domain.ErrInvalidArgument)
		}
		p.after = string(raw)
	}
	return p, nil
}

// paginate sorts items by key and returns the page after p.after, with
// the cursor of the next page ("" on the last one).
func paginate[T any](items []T, key func(T) string, p page) ([]T, string) {
	sort.SliceStable(items, func(i, j int) bool { return key(items[i]) < key(items[j]) })
	start := 0
	if p.after != "" {
		start = sort.Search(len(items), func(i int) bool { return key(items[i]) > p.after })
	}
	end := min(start+p.limit, len(items))
	out := items[start:end]
	next := ""
	if end < len(items) && len(out) > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(key(out[len(out)-1])))
	}
	return out, next
}

// trimList splits a comma separated query value.
func trimList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
