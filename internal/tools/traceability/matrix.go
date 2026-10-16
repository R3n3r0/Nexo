package traceability

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// OutputPath is the generated matrix, relative to the repository root.
const OutputPath = "test/TRACEABILITY.md"

// maxListed is the number of tests listed for a feature in the matrix; the
// others are counted.
const maxListed = 8

// Matrix maps the features of the catalog to their tests.
type Matrix struct {
	// Features are the features of the catalog, in catalog order.
	Features []Feature
	// Tests are the references of the tests of each code.
	Tests map[string][]Reference
}

// Build pairs the catalog with the references found by Scan.
func Build(features []Feature, refs map[string][]Reference) Matrix {
	return Matrix{Features: features, Tests: refs}
}

// Generate reads the catalog under root and scans its tests.
func Generate(root string) (Matrix, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return Matrix{}, fmt.Errorf("traceability: %w", err)
	}
	defer func() { _ = dir.Close() }()
	details := map[string]string{}
	for _, p := range DetailPaths() {
		f, err := dir.Open(filepath.FromSlash(p))
		if err != nil {
			return Matrix{}, fmt.Errorf("traceability: %w", err)
		}
		d, err := ParseDetails(f)
		_ = f.Close()
		if err != nil {
			return Matrix{}, err
		}
		for k, v := range d {
			details[k] = v
		}
	}
	f, err := dir.Open(filepath.FromSlash(CatalogPath))
	if err != nil {
		return Matrix{}, fmt.Errorf("traceability: %w", err)
	}
	defer func() { _ = f.Close() }()
	features, err := ParseCatalog(f, details)
	if err != nil {
		return Matrix{}, err
	}
	refs, err := Scan(root)
	if err != nil {
		return Matrix{}, err
	}
	return Build(features, refs), nil
}

// Missing returns the features in the scope of v1.0 (P0/P1) without tests.
func (m Matrix) Missing() []Feature {
	var out []Feature
	for _, f := range m.Features {
		if f.InScope() && len(m.Tests[f.Code]) == 0 {
			out = append(out, f)
		}
	}
	return out
}

// Unknown returns the codes named by tags that are not in the catalog, with
// their references.
func (m Matrix) Unknown() map[string][]Reference {
	known := make(map[string]bool, len(m.Features))
	for _, f := range m.Features {
		known[f.Code] = true
	}
	out := map[string][]Reference{}
	for code, refs := range m.Tests {
		if !known[code] {
			out[code] = refs
		}
	}
	return out
}

// docLink matches a relative Markdown link of the catalog
// ("[09](09-presigned.md)").
func docLink() *regexp.Regexp {
	return regexp.MustCompile(`\]\(([0-9][^)]*\.md[^)]*)\)`)
}

// cell makes a text fit a table cell of test/TRACEABILITY.md: pipes escaped,
// links of the catalog made relative to test/.
func cell(s string) string {
	s = docLink().ReplaceAllString(s, "](../docs/$1)")
	return strings.ReplaceAll(s, "|", `\|`)
}

// testsCell lists the tests of a code, at most maxListed, and counts them.
func (m Matrix) testsCell(code string) string {
	refs := m.Tests[code]
	if len(refs) == 0 {
		return "—"
	}
	n := min(len(refs), maxListed)
	parts := make([]string, 0, n+1)
	for _, r := range refs[:n] {
		parts = append(parts, cell(r.String()))
	}
	if len(refs) > n {
		parts = append(parts, fmt.Sprintf("… e altri %d", len(refs)-n))
	}
	return fmt.Sprintf("**%d**: %s", len(refs), strings.Join(parts, "<br>"))
}

// Markdown renders the matrix: a summary per priority, the P0/P1 features
// by area with their tests, the P2 features (outside v1.0) and the problems
// (P0/P1 features without tests, tags naming unknown codes).
func (m Matrix) Markdown() []byte {
	var b bytes.Buffer
	b.WriteString("# Matrice di tracciabilità\n\n")
	b.WriteString("<!-- Generato da `make traceability` (go run ./cmd/tools/traceability): non modificare a mano. -->\n\n")
	b.WriteString("Ogni funzionalità del [catalogo](../docs/02-features.md) con i test che la coprono " +
		"([06 §6.8](../docs/06-test-plan.md#68-matrice-di-tracciabilità)): un test dichiara le funzionalità " +
		"che verifica con un tag `covers: <codice>` in un commento (test Go, script `testscript` in `test/cli`, " +
		"test Vitest e Playwright in `web/`, script di test). I codici raggruppati del catalogo " +
		"(`D-ACC-01..04`, `D-CCH-01..08`, `D-DST-01..08`) sono espansi con le descrizioni di " +
		"[10](../docs/10-domains-access-distributions.md) e [11](../docs/11-domain-cache.md). " +
		"Il test `internal/tools/traceability` fallisce se una funzionalità P0/P1 non ha test, se un tag " +
		"nomina un codice fuori catalogo o se questo file non è aggiornato.\n\n")

	type count struct{ total, covered int }
	counts := map[string]*count{"P0": {}, "P1": {}, "P2": {}}
	for _, f := range m.Features {
		c := counts[f.Level()]
		if c == nil {
			continue
		}
		c.total++
		if len(m.Tests[f.Code]) > 0 {
			c.covered++
		}
	}
	b.WriteString("## Riepilogo\n\n| Priorità | Funzionalità | Con test | Senza test |\n|----------|-------------:|---------:|-----------:|\n")
	for _, p := range []string{"P0", "P1"} {
		c := counts[p]
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", p, c.total, c.covered, c.total-c.covered)
	}
	c := counts["P2"]
	fmt.Fprintf(&b, "| P2 (non in v1.0) | %d | %d | — |\n\n", c.total, c.covered)
	b.WriteString("Una funzionalità con priorità mista (`P0/P1`, `P1 / P2`) è nel perimetro della v1.0 per la sua " +
		"parte P0/P1 ed è contata con la priorità più alta.\n\n")

	missing := m.Missing()
	unknown := m.Unknown()
	if len(missing) > 0 || len(unknown) > 0 {
		b.WriteString("## Problemi\n\n")
		for _, f := range missing {
			fmt.Fprintf(&b, "- `%s` (%s) senza test\n", f.Code, f.Priority)
		}
		codes := make([]string, 0, len(unknown))
		for code := range unknown {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			refs := make([]string, 0, len(unknown[code]))
			for _, r := range unknown[code] {
				refs = append(refs, fmt.Sprintf("`%s:%d`", r.Path, r.Line))
			}
			fmt.Fprintf(&b, "- codice fuori catalogo `%s` in %s\n", code, strings.Join(refs, ", "))
		}
		b.WriteString("\n")
	}

	area := ""
	for _, f := range m.Features {
		if !f.InScope() {
			continue
		}
		if f.Area != area {
			area = f.Area
			fmt.Fprintf(&b, "## %s\n\n| Codice | Funzionalità | Priorità | Test |\n|--------|--------------|----------|------|\n", area)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", f.Code, cell(f.Name), cell(f.Priority), m.testsCell(f.Code))
		if next := m.nextInScope(f); next == nil || next.Area != area {
			b.WriteString("\n")
		}
	}

	b.WriteString("## P2 — non in v1.0 (M10)\n\n")
	b.WriteString("Funzionalità successive alla v1.0 ([05, M10](../docs/05-roadmap.md#m10--post-v10-p2)): " +
		"non fanno parte del perimetro della v1.0; i test elencati coprono le parti già presenti.\n\n")
	b.WriteString("| Codice | Area | Funzionalità | Test |\n|--------|------|--------------|------|\n")
	for _, f := range m.Features {
		if f.InScope() {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", f.Code, cell(f.Area), cell(f.Name), m.testsCell(f.Code))
	}
	return b.Bytes()
}

// nextInScope returns the P0/P1 feature that follows f in the catalog.
func (m Matrix) nextInScope(f Feature) *Feature {
	found := false
	for i := range m.Features {
		if found && m.Features[i].InScope() {
			return &m.Features[i]
		}
		if m.Features[i].Code == f.Code {
			found = true
		}
	}
	return nil
}
