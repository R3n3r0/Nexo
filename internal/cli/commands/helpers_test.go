package commands_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/internal/cli/commands"
	"github.com/r3n3r0/nexo/internal/clock"
)

// update rewrites the golden files: go test ./internal/cli/commands -update
var update = flag.Bool("update", false, "rewrite the golden files of testdata/golden")

// t0 is the date of the fake clock of the tests.
var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// harness runs nexoctl in-process against an in-memory S3 store (alias
// "mem"), in a temporary working directory that also holds the
// configuration.
type harness struct {
	t     *testing.T
	mem   *clitest.MemS3
	admin *clitest.MemAdmin
	clock *clock.Fake
	dir   string
	deps  commands.Deps
	tty   bool
	stdin string
	env   map[string]string

	mu    sync.Mutex
	execs []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := clock.NewFake(t0)
	h := &harness{t: t, mem: clitest.NewMemS3(clk), admin: clitest.NewMemAdmin(clk), clock: clk, dir: t.TempDir(), env: map[string]string{}}
	h.env[cli.EnvConfigDir] = filepath.Join(h.dir, ".nexoctl")
	t.Chdir(h.dir)
	h.deps = commands.Deps{
		LookupEnv: func(k string) (string, bool) {
			v, ok := h.env[k]
			return v, ok
		},
		S3:         h.mem.Factory(t),
		Admin:      h.admin.Factory(t),
		Clock:      clk,
		IsTerminal: func(io.Writer) bool { return h.tty },
		HomeDir:    func() (string, error) { return h.dir, nil },
		Exec: func(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			line := strings.Join(append([]string{name}, args...), " ")
			h.execs = append(h.execs, line)
			_, err := fmt.Fprintln(stdout, "exec:", line)
			return err
		},
		WatchDebounce: 20 * time.Millisecond,
	}
	h.mustRun("alias", "set", "mem", "http://s3.mem.test", clitest.MemAdminRoot, "MEMSECRET")
	return h
}

// result is the outcome of one run.
type result struct {
	stdout string
	stderr string
	code   int
}

// run executes nexoctl with args.
func (h *harness) run(args ...string) result {
	h.t.Helper()
	return h.runContext(context.Background(), args...)
}

func (h *harness) runContext(ctx context.Context, args ...string) result {
	h.t.Helper()
	var out, errOut bytes.Buffer
	deps := h.deps
	deps.Stdout, deps.Stderr, deps.Stdin = &out, &errOut, strings.NewReader(h.stdin)
	code := commands.Main(ctx, deps, args)
	return result{stdout: out.String(), stderr: errOut.String(), code: code}
}

// mustRun executes nexoctl and requires success.
func (h *harness) mustRun(args ...string) result {
	h.t.Helper()
	r := h.run(args...)
	require.Equal(h.t, 0, r.code, "nexoctl %s\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.stdout, r.stderr)
	return r
}

// write creates a local file with a fixed modification time.
func (h *harness) write(rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.dir, filepath.FromSlash(rel))
	require.NoError(h.t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(h.t, os.WriteFile(p, []byte(content), 0o600))
	require.NoError(h.t, os.Chtimes(p, t0, t0))
}

func (h *harness) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, filepath.FromSlash(rel)))
	require.NoError(h.t, err)
	return string(b)
}

// session runs commands and renders a transcript: the command line, its
// stdout, its stderr (prefixed with "! ") and the exit code when not 0.
type session struct {
	h *harness
	b strings.Builder
}

func (h *harness) session() *session { return &session{h: h} }

func (s *session) run(args ...string) result {
	s.h.t.Helper()
	r := s.h.run(args...)
	fmt.Fprintf(&s.b, "$ nexoctl %s\n", strings.Join(args, " "))
	s.b.WriteString(r.stdout)
	if r.stderr != "" {
		for _, line := range strings.Split(strings.TrimRight(r.stderr, "\n"), "\n") {
			s.b.WriteString("! " + line + "\n")
		}
	}
	if r.code != 0 {
		fmt.Fprintf(&s.b, "[exit %d]\n", r.code)
	}
	s.b.WriteString("\n")
	return r
}

// golden compares got with testdata/golden/name.golden (rewritten with
// -update).
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir(t), filepath.FromSlash(name)+".golden")
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden file: run go test ./internal/cli/commands -update")
	assert.Equal(t, string(want), got, "golden file %s (run with -update to accept the changes)", path)
}

// goldenDir is testdata/golden of the package (the tests change their
// working directory).
func goldenDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(packageDir, "testdata", "golden")
}

// packageDir is the directory of the package, read before any test changes
// the working directory.
var packageDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}()

// jsonLines checks that out is JSON Lines whose objects start with
// "status" and returns them.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		require.True(t, strings.HasPrefix(line, `{"status":"`), "status first: %q", line)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "invalid JSON line %q", line)
		recs = append(recs, m)
	}
	return recs
}
