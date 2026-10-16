// Package traceability builds the traceability matrix of docs/06 §6.8: every
// feature of the catalog (docs/02-features.md) mapped to the tests that
// declare to cover it with a "covers:" tag (Go tests, testscript scripts,
// Vitest and Playwright specs, test scripts).
//
// The matrix is written to test/TRACEABILITY.md by cmd/tools/traceability;
// the test of this package fails when a P0/P1 feature has no test, when a
// tag names a code that is not in the catalog or when the committed matrix
// is stale. The package is a development tool: no binary of the release
// imports it.
package traceability

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// CatalogPath is the feature catalog, relative to the repository root.
const CatalogPath = "docs/02-features.md"

// DetailPaths are the documents that detail the codes the catalog groups in
// a range (D-ACC-01..04 in docs/10, D-CCH-01..08 in docs/11).
func DetailPaths() []string {
	return []string{"docs/10-domains-access-distributions.md", "docs/11-domain-cache.md"}
}

// Feature is one code of the catalog.
type Feature struct {
	// Code is the feature code (S-OBJ-06, D-ACC-02, C-ADM-08).
	Code string
	// Area is the section of the catalog ("Server — Oggetti", "Dashboard").
	Area string
	// Name is the description of the feature.
	Name string
	// Priority is the priority as the catalog writes it ("P0", "P0/P1",
	// "P1 / P2").
	Priority string
}

// InScope reports whether the feature belongs to v1.0: its priority names
// P0 or P1 (a feature whose priority also names P2 is in scope for its P0/P1
// part).
func (f Feature) InScope() bool {
	return strings.Contains(f.Priority, "P0") || strings.Contains(f.Priority, "P1")
}

// Level is the highest priority of the feature: "P0", "P1" or "P2".
func (f Feature) Level() string {
	for _, p := range []string{"P0", "P1", "P2"} {
		if strings.Contains(f.Priority, p) {
			return p
		}
	}
	return f.Priority
}

// codePattern matches a feature code, optionally a range of codes
// (D-ACC-01..04).
func codePattern() *regexp.Regexp {
	return regexp.MustCompile(`\b([SDC]-[A-Z]{2,4}-)(\d{2})([a-z]?)(?:\.\.(\d{2}))?\b`)
}

// expandCodes returns the codes s names, ranges expanded.
func expandCodes(s string) []string {
	var out []string
	for _, m := range codePattern().FindAllStringSubmatch(s, -1) {
		if m[4] == "" {
			out = append(out, m[1]+m[2]+m[3])
			continue
		}
		from, _ := strconv.Atoi(m[2])
		to, _ := strconv.Atoi(m[4])
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("%s%02d", m[1], i))
		}
	}
	return out
}

// tableCells splits a Markdown table row into its trimmed cells; ok is false
// for a line that is not a row.
func tableCells(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") || len(line) < 2 {
		return nil, false
	}
	cells := strings.Split(line[1:len(line)-1], "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells, true
}

// ParseDetails reads the tables of a document that describe single codes
// ("| D-ACC-01 | description |") and returns the description of each code.
func ParseDetails(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		cells, ok := tableCells(sc.Text())
		if !ok || len(cells) < 2 {
			continue
		}
		codes := expandCodes(cells[0])
		if len(codes) != 1 || codes[0] != cells[0] {
			continue
		}
		out[codes[0]] = cells[1]
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("traceability: read details: %w", err)
	}
	return out, nil
}

// ParseCatalog reads the feature tables of the catalog: the first cell of a
// row is the code (or a range of codes, expanded with the descriptions of
// details), the last one the priority, the cells between them the name.
// The area of a feature is the section ("## 2.1 Server") and the
// subsection ("### Bucket") that hold its table.
func ParseCatalog(r io.Reader, details map[string]string) ([]Feature, error) {
	var (
		out     []Feature
		section string
		sub     string
		seen    = map[string]bool{}
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			section, sub = headingTitle(line[3:]), ""
			continue
		case strings.HasPrefix(line, "### "):
			sub = headingTitle(line[4:])
			continue
		}
		cells, ok := tableCells(line)
		if !ok || len(cells) < 3 {
			continue
		}
		codes := expandCodes(cells[0])
		if len(codes) == 0 || strings.TrimSpace(codePattern().ReplaceAllString(cells[0], "")) != "" {
			continue
		}
		priority := cells[len(cells)-1]
		var parts []string
		for _, c := range cells[1 : len(cells)-1] {
			if c != "" {
				parts = append(parts, c)
			}
		}
		name := strings.Join(parts, " — ")
		area := section
		if sub != "" {
			area += " — " + sub
		}
		for _, code := range codes {
			if seen[code] {
				return nil, fmt.Errorf("traceability: code %s appears twice in the catalog", code)
			}
			seen[code] = true
			f := Feature{Code: code, Area: area, Name: name, Priority: priority}
			if len(codes) > 1 {
				if d, ok := details[code]; ok {
					f.Name = parts[0] + " — " + d
				}
			}
			out = append(out, f)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("traceability: read catalog: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("traceability: the catalog has no feature")
	}
	return out, nil
}

// headingTitle drops the number of a section and its cross references
// ("2.1 Server" → "Server", "Cache (vedi [11](...))" → "Cache").
func headingTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i > 0 && strings.Trim(s[:i], "0123456789.") == "" {
		s = s[i+1:]
	}
	if i := strings.Index(s, " (vedi "); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
