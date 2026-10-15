package traceability

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Reference is a test that declares to cover a feature.
type Reference struct {
	// Path is the slash-separated path of the file, relative to the root.
	Path string
	// Line is the line of the "covers:" tag.
	Line int
	// Test names the test that holds the tag: the Go function, the title of
	// the Vitest/Playwright test or suite; empty when the tag covers the
	// whole file (a testscript script, a test script).
	Test string
}

// String formats the reference for the matrix.
func (r Reference) String() string {
	if r.Test == "" {
		return "`" + r.Path + "`"
	}
	return "`" + r.Path + "` " + r.Test
}

// skipDirs are the directories never scanned: dependencies, build output,
// version control and this package, whose tests hold tags as fixtures.
func skipDirs() map[string]bool {
	return map[string]bool{
		".git": true, "node_modules": true, "web/dist": true, "bin": true,
		"web/test-results": true, "web/playwright-report": true,
		"internal/tools/traceability": true,
	}
}

// isTestSource reports whether path (slash-separated, relative to the root)
// is a test source whose tags count: Go tests, testscript scripts, Vitest
// tests, Playwright specs and the test scripts of the release (shell and
// Python files).
func isTestSource(path string) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasSuffix(base, ".txtar"):
		return true
	case strings.HasPrefix(path, "web/"):
		return strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".test.tsx") ||
			strings.HasSuffix(base, ".spec.ts")
	case strings.HasSuffix(base, ".sh") || strings.HasSuffix(base, ".py"):
		return true
	}
	return false
}

// tagPattern matches a "covers:" tag in a comment (//, #, or a line of a
// block comment starting with *) and captures its value.
func tagPattern() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|\s)(?://|#|\*)\s*covers:\s*(.*)$`)
}

// commentText returns the text of a comment line (without //, # or *) and
// whether the line is a comment.
func commentText(line string) (string, bool) {
	t := strings.TrimSpace(line)
	for _, p := range []string{"//", "#", "*"} {
		if strings.HasPrefix(t, p) {
			return strings.TrimSpace(strings.TrimPrefix(t, p)), true
		}
	}
	return "", false
}

// tag is a "covers:" tag found in a file.
type tag struct {
	line  int
	codes []string
}

// fileTags returns the tags of a file. A tag whose value ends with a comma
// continues on the next comment line.
func fileTags(src []byte) []tag {
	var (
		out   []tag
		lines []string
	)
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	re := tagPattern()
	for i := 0; i < len(lines); i++ {
		m := re.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		value := strings.TrimSpace(m[1])
		for j := i + 1; strings.HasSuffix(value, ",") && j < len(lines); j++ {
			next, ok := commentText(lines[j])
			if !ok {
				break
			}
			value += " " + next
		}
		if codes := expandCodes(value); len(codes) > 0 {
			out = append(out, tag{line: i + 1, codes: codes})
		}
	}
	return out
}

// goTestNames returns, for each tag of a Go file, the function whose doc
// comment or body holds it ("" outside any function).
func goTestNames(path string, src []byte, tags []tag) []string {
	names := make([]string, len(tags))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return names
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		if fn.Doc != nil {
			start = fset.Position(fn.Doc.Pos()).Line
		}
		end := fset.Position(fn.End()).Line
		for i, t := range tags {
			if t.line >= start && t.line <= end {
				names[i] = fn.Name.Name
			}
		}
	}
	return names
}

// jsTestPattern matches the opening of a Vitest or Playwright test or suite
// and captures its title.
func jsTestPattern() *regexp.Regexp {
	return regexp.MustCompile("^\\s*(?:test|it|describe)(?:\\.[a-z]+)*\\(\\s*(['\"`])(.*?)['\"`]")
}

// jsTestNames returns, for each tag of a TypeScript test, the title of the
// first test or suite that follows it within a few lines ("" otherwise:
// the tag covers the file).
func jsTestNames(src []byte, tags []tag) []string {
	lines := strings.Split(string(src), "\n")
	re := jsTestPattern()
	names := make([]string, len(tags))
	for i, t := range tags {
		for j := t.line; j < len(lines) && j < t.line+6; j++ {
			if m := re.FindStringSubmatch(lines[j]); m != nil {
				names[i] = strings.TrimSpace(m[2])
				break
			}
		}
	}
	return names
}

// Scan walks the test sources under root and returns, for each feature
// code, the references of the tests that cover it, sorted by path and line.
func Scan(root string) (map[string][]Reference, error) {
	out := map[string][]Reference{}
	skip := skipDirs()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skip[rel] || skip[d.Name()] && d.Name() != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !isTestSource(rel) {
			return nil
		}
		src, err := os.ReadFile(path) // #nosec G304,G122 -- a test source of the repository being scanned
		if err != nil {
			return fmt.Errorf("traceability: %w", err)
		}
		tags := fileTags(src)
		if len(tags) == 0 {
			return nil
		}
		var names []string
		switch {
		case strings.HasSuffix(rel, ".go"):
			names = goTestNames(rel, src, tags)
		case strings.HasSuffix(rel, ".ts") || strings.HasSuffix(rel, ".tsx"):
			names = jsTestNames(src, tags)
		default:
			names = make([]string, len(tags))
		}
		for i, t := range tags {
			for _, code := range t.codes {
				out[code] = append(out[code], Reference{Path: rel, Line: t.line, Test: names[i]})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("traceability: scan %s: %w", root, err)
	}
	for code, refs := range out {
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].Path != refs[j].Path {
				return refs[i].Path < refs[j].Path
			}
			return refs[i].Line < refs[j].Line
		})
		out[code] = dedupe(refs)
	}
	return out, nil
}

// dedupe drops the repeated references of one code to one test (a test
// whose doc comment and body both tag the code).
func dedupe(refs []Reference) []Reference {
	out := refs[:0]
	for i, r := range refs {
		if i > 0 && r.Test != "" && r.Path == refs[i-1].Path && r.Test == refs[i-1].Test {
			continue
		}
		out = append(out, r)
	}
	return out
}
