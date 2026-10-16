package traceability_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/tools/traceability"
)

// tag builds a "covers:" tag without writing it literally in this file.
func tag(value string) string { return "cov" + "ers: " + value }

func TestParseCatalog(t *testing.T) {
	doc := strings.Join([]string{
		"# 2. Catalog",
		"## 2.1 Server",
		"### Bucket",
		"| Code | Feature | Priority |",
		"|--------|--------------|----------|",
		"| S-BKT-01 | CreateBucket | P0 |",
		"| S-BKT-12 | Replication | P2 |",
		"### Rules (see [10](10-domains.md))",
		"| S-DST-07c | Shared cache | P1 / P2 |",
		"## 2.2 Dashboard (Console React)",
		"| Code | Page | Detail | Priority |",
		"| D-ACC-01..02 | Access rules | Editor | P0 |",
		"| D-UX-01 | Theme | | P1 |",
		"## 2.4 Differences",
		"| Area | Behavior | Reason |",
		"| ACL | S-BKT-13 not supported | P2 |",
	}, "\n")
	details := map[string]string{"D-ACC-01": "Rules table", "D-ACC-02": "Tester"}
	got, err := traceability.ParseCatalog(strings.NewReader(doc), details)
	require.NoError(t, err)
	want := []traceability.Feature{
		{Code: "S-BKT-01", Area: "Server — Bucket", Name: "CreateBucket", Priority: "P0"},
		{Code: "S-BKT-12", Area: "Server — Bucket", Name: "Replication", Priority: "P2"},
		{Code: "S-DST-07c", Area: "Server — Rules", Name: "Shared cache", Priority: "P1 / P2"},
		{Code: "D-ACC-01", Area: "Dashboard (Console React)", Name: "Access rules — Rules table", Priority: "P0"},
		{Code: "D-ACC-02", Area: "Dashboard (Console React)", Name: "Access rules — Tester", Priority: "P0"},
		{Code: "D-UX-01", Area: "Dashboard (Console React)", Name: "Theme", Priority: "P1"},
	}
	assert.Equal(t, want, got)
}

func TestParseCatalogErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{name: "no feature", doc: "# nothing\n| a | b | c |\n"},
		{name: "duplicate code", doc: "| S-OBJ-01 | a | P0 |\n| S-OBJ-01 | b | P0 |\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := traceability.ParseCatalog(strings.NewReader(tt.doc), nil)
			assert.Error(t, err)
		})
	}
}

func TestFeatureScope(t *testing.T) {
	tests := []struct {
		priority string
		inScope  bool
		level    string
	}{
		{priority: "P0", inScope: true, level: "P0"},
		{priority: "P1", inScope: true, level: "P1"},
		{priority: "P0/P1", inScope: true, level: "P0"},
		{priority: "P1 / P2", inScope: true, level: "P1"},
		{priority: "P2 / P1", inScope: true, level: "P1"},
		{priority: "P2", inScope: false, level: "P2"},
	}
	for _, tt := range tests {
		t.Run(tt.priority, func(t *testing.T) {
			f := traceability.Feature{Code: "S-X-01", Priority: tt.priority}
			assert.Equal(t, tt.inScope, f.InScope())
			assert.Equal(t, tt.level, f.Level())
		})
	}
}

func TestParseDetails(t *testing.T) {
	doc := "| D-CCH-01 | Global page |\n| D-CCH-02 | Cache tab |\n| D-CCH-01..08 | group |\n| text | other |\n"
	got, err := traceability.ParseDetails(strings.NewReader(doc))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"D-CCH-01": "Global page", "D-CCH-02": "Cache tab"}, got)
}

// writeFiles creates the files of a test repository.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"internal/x/x_test.go": "package x\n\n// TestA is a test.\n//\n// " + tag("S-OBJ-01, S-OBJ-02 (note)") +
			"\nfunc TestA(t *testing.T) {\n\t// " + tag("S-OBJ-01") + "\n}\n\nfunc TestB(t *testing.T) {\n\t// " +
			tag("S-OBJ-03,") + "\n\t// S-OBJ-04\n}\n",
		"internal/x/x.go":                             "package x\n// " + tag("S-OBJ-09") + "\n",
		"test/cli/a.txtar":                            "# " + tag("C-OBJ-01") + "\nexec nexoctl ls\n",
		"web/src/a.test.tsx":                          "describe('x', () => {\n  // " + tag("D-ACC-01..03") + "\n  it('TD-A-01 does it', () => {})\n})\n",
		"web/e2e/b.spec.ts":                           "// " + tag("D-UX-01") + "\ntest.describe('suite', () => {})\n",
		"web/src/notatest.ts":                         "// " + tag("D-UX-02") + "\n",
		"packaging/smoke.sh":                          "#!/bin/sh\n# " + tag("S-OPS-12") + "\n",
		"web/node_modules/m/m_test.go":                "// " + tag("S-OPS-13") + "\n",
		"internal/tools/traceability/fixture_test.go": "// " + tag("S-OPS-14") + "\n",
	})
	got, err := traceability.Scan(root)
	require.NoError(t, err)
	want := map[string][]traceability.Reference{
		"S-OBJ-01": {{Path: "internal/x/x_test.go", Line: 5, Test: "TestA"}},
		"S-OBJ-02": {{Path: "internal/x/x_test.go", Line: 5, Test: "TestA"}},
		"S-OBJ-03": {{Path: "internal/x/x_test.go", Line: 11, Test: "TestB"}},
		"S-OBJ-04": {{Path: "internal/x/x_test.go", Line: 11, Test: "TestB"}},
		"C-OBJ-01": {{Path: "test/cli/a.txtar", Line: 1}},
		"D-ACC-01": {{Path: "web/src/a.test.tsx", Line: 2, Test: "TD-A-01 does it"}},
		"D-ACC-02": {{Path: "web/src/a.test.tsx", Line: 2, Test: "TD-A-01 does it"}},
		"D-ACC-03": {{Path: "web/src/a.test.tsx", Line: 2, Test: "TD-A-01 does it"}},
		"D-UX-01":  {{Path: "web/e2e/b.spec.ts", Line: 1, Test: "suite"}},
		"S-OPS-12": {{Path: "packaging/smoke.sh", Line: 2}},
	}
	assert.Equal(t, want, got)
}

func TestMatrix(t *testing.T) {
	features := []traceability.Feature{
		{Code: "S-OBJ-01", Area: "Server — Objects", Name: "Put | Get", Priority: "P0"},
		{Code: "S-OBJ-02", Area: "Server — Objects", Name: "Range ([09](09-presigned.md))", Priority: "P1"},
		{Code: "S-OBJ-14", Area: "Server — Objects", Name: "Compression", Priority: "P2"},
	}
	refs := map[string][]traceability.Reference{
		"S-OBJ-01": {{Path: "a_test.go", Line: 3, Test: "TestA"}},
		"S-XYZ-01": {{Path: "b_test.go", Line: 7, Test: "TestB"}},
	}
	m := traceability.Build(features, refs)
	assert.Equal(t, []traceability.Feature{features[1]}, m.Missing())
	assert.Equal(t, map[string][]traceability.Reference{"S-XYZ-01": refs["S-XYZ-01"]}, m.Unknown())

	doc := string(m.Markdown())
	assert.Contains(t, doc, "| P0 | 1 | 1 | 0 |")
	assert.Contains(t, doc, "| P1 | 1 | 0 | 1 |")
	assert.Contains(t, doc, "| P2 (not in v1.0) | 1 | 0 | — |")
	assert.Contains(t, doc, "| S-OBJ-01 | Put \\| Get | P0 | **1**: `a_test.go` TestA |")
	assert.Contains(t, doc, "[09](../docs/09-presigned.md)", "the links of the catalog are relative to test/")
	assert.Contains(t, doc, "- `S-OBJ-02` (P1) without tests")
	assert.Contains(t, doc, "- code outside the catalog `S-XYZ-01` in `b_test.go:7`")
	assert.Contains(t, doc, "| S-OBJ-14 | Server — Objects | Compression | — |")
	assert.Equal(t, doc, string(m.Markdown()), "the matrix is deterministic")
}

// TestRepositoryTraceability is the check of docs/06 §6.8 on this
// repository: every P0/P1 feature of the catalog has at least one test,
// every tag names a code of the catalog and test/TRACEABILITY.md is up to
// date (make traceability rewrites it).
func TestRepositoryTraceability(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	m, err := traceability.Generate(root)
	require.NoError(t, err)
	for _, f := range m.Missing() {
		t.Errorf("%s (%s, %s): no test covers it; add a %q tag to the tests that verify it", f.Code, f.Priority, f.Name, tag(f.Code))
	}
	for code, refs := range m.Unknown() {
		for _, r := range refs {
			t.Errorf("%s:%d: %s is not a code of %s", r.Path, r.Line, code, traceability.CatalogPath)
		}
	}
	current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(traceability.OutputPath)))
	require.NoError(t, err)
	assert.True(t, string(current) == string(m.Markdown()), "%s is stale: run make traceability", traceability.OutputPath)
}
