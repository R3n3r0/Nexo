package nexoadmin_test

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/stretchr/testify/require"
)

// specPath is the OpenAPI document of the Admin API.
const specPath = "../../api/openapi.yaml"

// contractTransport validates every request and response it carries
// against api/openapi.yaml (docs/06: client nexoadmin vs OpenAPI) and
// records the operations exercised.
type contractTransport struct {
	t      testing.TB
	router routers.Router
	next   http.RoundTripper

	mu  sync.Mutex
	ops map[string]bool
}

// newContractTransport loads the document with the server URL of the
// console listener.
func newContractTransport(t testing.TB, consoleURL string) *contractTransport {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specPath)
	require.NoError(t, err)
	require.NoError(t, doc.Validate(context.Background()))
	doc.Servers = openapi3.Servers{{URL: consoleURL + "/api/v1"}}
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	return &contractTransport{t: t, router: router, next: http.DefaultTransport, ops: map[string]bool{}}
}

// jsonBody reports a JSON content type (the only bodies validated: the
// raw uploads are streamed).
func jsonBody(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mt == "application/json" || mt == "application/problem+json")
}

// RoundTrip implements http.RoundTripper.
func (c *contractTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	route, params, err := c.router.FindRoute(req)
	if err != nil {
		c.t.Errorf("contract: %s %s: no operation in the document: %v", req.Method, req.URL.Path, err)
		return c.next.RoundTrip(req)
	}
	c.mu.Lock()
	c.ops[route.Operation.OperationID] = true
	c.mu.Unlock()

	validateBody := jsonBody(req.Header.Get("Content-Type"))
	var body []byte
	if validateBody && req.Body != nil {
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	check := req.Clone(req.Context())
	check.Body = io.NopCloser(bytes.NewReader(body))
	in := &openapi3filter.RequestValidationInput{
		Request: check, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: !validateBody, MultiError: true},
	}
	if err := openapi3filter.ValidateRequest(req.Context(), in); err != nil {
		c.t.Errorf("contract: request %s %s (%s): %v", req.Method, req.URL.Path, route.Operation.OperationID, err)
	}

	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: resp.StatusCode, Header: resp.Header,
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true, ExcludeResponseBody: !jsonBody(resp.Header.Get("Content-Type"))},
	}
	out.SetBodyBytes(data)
	if err := openapi3filter.ValidateResponse(req.Context(), out); err != nil {
		c.t.Errorf("contract: response %d of %s %s (%s): %v\n%s", resp.StatusCode, req.Method, req.URL.Path, route.Operation.OperationID, err, data)
	}
	return resp, nil
}

// operations returns the operation ids exercised, sorted.
func (c *contractTransport) operations() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.ops))
	for op := range c.ops {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}
