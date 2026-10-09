package cli_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/rogpeppe/go-internal/testscript"
)

// openAPISpec is api/openapi.yaml, loaded once.
var openAPISpec = sync.OnceValues(func() (*openapi3.T, error) {
	path, err := filepath.Abs(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		return nil, err
	}
	doc, err := openapi3.NewLoader().LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	return doc, doc.Validate(context.Background())
})

// cmdOpenAPISchema validates the member MEMBER of the first JSON line of
// FILE against the schema SCHEMA of the Admin API document (TCL-C-05: the
// JSON of the CLI is the one of the API).
func cmdOpenAPISchema(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: openapischema FILE SCHEMA MEMBER")
	}
	doc, err := openAPISpec()
	ts.Check(err)
	ref, ok := doc.Components.Schemas[args[1]]
	if !ok || ref.Value == nil {
		ts.Fatalf("openapischema: no schema %q in api/openapi.yaml", args[1])
	}
	first, _, _ := strings.Cut(ts.ReadFile(args[0]), "\n")
	var rec map[string]any
	ts.Check(json.Unmarshal([]byte(first), &rec))
	v, ok := rec[args[2]]
	if !ok {
		ts.Fatalf("openapischema: no member %q in %s", args[2], first)
	}
	if err := ref.Value.VisitJSON(v, openapi3.MultiErrors()); err != nil {
		ts.Fatalf("openapischema: %s does not match %s: %v", args[2], args[1], err)
	}
}
