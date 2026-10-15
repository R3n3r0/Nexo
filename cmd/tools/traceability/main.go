// Command traceability writes the traceability matrix of docs/06 §6.8
// (test/TRACEABILITY.md): every feature of the catalog with the tests that
// cover it. With -check it writes nothing and fails when a P0/P1 feature has
// no test, when a tag names a code outside the catalog or when the matrix
// on disk is stale. It is a development tool, never part of a release.
//
//	go run ./cmd/tools/traceability            # write test/TRACEABILITY.md
//	go run ./cmd/tools/traceability -check     # verify it (CI)
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/r3n3r0/nexo/internal/tools/traceability"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("traceability", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "root of the repository")
	out := fs.String("o", traceability.OutputPath, "matrix to write, relative to -root")
	check := fs.Bool("check", false, "verify the matrix instead of writing it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	m, err := traceability.Generate(*root)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	doc := m.Markdown()
	path := filepath.Join(*root, filepath.FromSlash(*out))
	failed := false
	for _, f := range m.Missing() {
		_, _ = fmt.Fprintf(stderr, "%s (%s): no test covers it\n", f.Code, f.Priority)
		failed = true
	}
	unknown := m.Unknown()
	codes := make([]string, 0, len(unknown))
	for code := range unknown {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		for _, r := range unknown[code] {
			_, _ = fmt.Fprintf(stderr, "%s:%d: %s is not a code of the catalog\n", r.Path, r.Line, code)
		}
		failed = true
	}
	if *check {
		current, err := os.ReadFile(path) // #nosec G304 -- the path given on the command line
		if err != nil || !bytes.Equal(current, doc) {
			_, _ = fmt.Fprintf(stderr, "%s is stale: run make traceability\n", *out)
			failed = true
		}
	} else {
		if err := os.WriteFile(path, doc, 0o644); err != nil { // #nosec G306 -- a document of the repository
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "wrote %s\n", *out)
	}
	if failed {
		return 1
	}
	return 0
}
