package commands_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/internal/cli/commands"
	"github.com/r3n3r0/nexo/internal/domain"
)

// covers: TCL-E-18, TCL-U-03
func TestEveryCommandPrintsJSONLines(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("bkt")
	h.mem.PutObject("bkt", "dir/a.json", `{"a":1}`, clitest.ObjectSpec{Metadata: map[string]string{"k": "v"}})
	h.mem.PutObject("bkt", "bin", "\xff\xfe", clitest.ObjectSpec{})
	h.mem.AddUpload("bkt", "up", "u1")
	h.write("local/f.json", "{}")
	h.write("policy.json", `{"Version":"2012-10-17","Statement":[]}`)
	h.stdin = "piped"
	commandsWithRecords := [][]string{
		{"alias", "set", "other", "http://x.test", "AK", "SK"},
		{"alias", "list"},
		{"ls", "mem"},
		{"ls", "-r", "mem/bkt"},
		{"ls", "mem/bkt"},
		{"ls", "--versions", "mem/bkt"},
		{"ls", "--incomplete", "mem/bkt"},
		{"ls", "local"},
		{"mb", "mem/new-bucket"},
		{"cp", "local/f.json", "mem/bkt/f.json"},
		{"cp", "-r", "mem/bkt/dir/", "down"},
		{"mv", "mem/bkt/f.json", "mem/bkt/g.json"},
		{"pipe", "mem/bkt/piped"},
		{"cat", "mem/bkt/dir/a.json"},
		{"cat", "mem/bkt/bin"},
		{"head", "-n", "1", "mem/bkt/dir/a.json"},
		{"stat", "mem/bkt/dir/a.json"},
		{"stat", "mem/bkt"},
		{"stat", "local/f.json"},
		{"mirror", "local", "mem/bkt/mirror"},
		{"find", "mem/bkt", "--name", "*.json"},
		{"du", "--depth", "1", "mem/bkt"},
		{"tree", "mem/bkt"},
		{"diff", "local", "mem/bkt/mirror2"},
		{"share", "download", "mem/bkt/dir/a.json"},
		{"share", "upload", "mem/bkt/up.bin"},
		{"share", "upload", "--post", "mem/bkt/inbox/"},
		{"presign", "-X", "HEAD", "mem/bkt/dir/a.json"},
		{"anonymous", "set", "download", "mem/bkt/dir/"},
		{"anonymous", "get", "mem/bkt/dir/"},
		{"anonymous", "list", "mem/bkt"},
		{"anonymous", "get-json", "mem/bkt"},
		{"anonymous", "set-json", "policy.json", "mem/bkt"},
		{"version", "enable", "mem/bkt"},
		{"version", "info", "mem/bkt"},
		{"version"},
		{"tag", "set", "mem/bkt/dir/a.json", "a=1"},
		{"tag", "list", "mem/bkt/dir/a.json"},
		{"tag", "remove", "mem/bkt/dir/a.json"},
		{"rm", "mem/bkt/g.json"},
		{"rm", "--incomplete", "mem/bkt/up"},
		{"rm", "-r", "--force", "--dry-run", "mem/bkt/dir"},
		{"rb", "--force", "mem/new-bucket"},
		{"alias", "remove", "other"},
	}
	for _, args := range commandsWithRecords {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := h.run(append(args, "--json")...)
			require.Equal(t, 0, r.code, "stderr: %s", r.stderr)
			recs := jsonLines(t, r.stdout)
			require.NotEmpty(t, recs, "every command prints at least one record")
			for _, rec := range recs {
				assert.Equal(t, "success", rec["status"])
			}
			for _, rec := range jsonLines(t, r.stderr) {
				assert.Contains(t, []string{"warning", "error"}, rec["status"])
			}
		})
	}

	// Errors are JSON too, on stderr.
	r := h.run("stat", "mem/bkt/missing", "--json")
	assert.Equal(t, cli.ExitNotFound, r.code)
	assert.Empty(t, r.stdout)
	errs := jsonLines(t, r.stderr)
	require.Len(t, errs, 1)
	assert.Equal(t, "error", errs[0]["status"])
	assert.Equal(t, map[string]any{"code": "NotFound", "message": "mem/bkt/missing: object not found: missing", "exitCode": float64(3)}, errs[0]["error"])
	r = h.run("cp", "--json")
	assert.Equal(t, cli.ExitUsage, r.code)
	assert.Equal(t, "UsageError", jsonLines(t, r.stderr)[0]["error"].(map[string]any)["code"])
	assert.NotContains(t, r.stderr, "--help", "no usage hint in JSON mode")
}

// covers: TCL-U-04
func TestExitCodes(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("bkt")
	h.mem.PutObject("bkt", "k", "v", clitest.ObjectSpec{})
	tests := []struct {
		name string
		fail map[string]error
		args []string
		code int
	}{
		{"ok", nil, []string{"ls", "mem/bkt"}, cli.ExitOK},
		{"help", nil, []string{"--help"}, cli.ExitOK},
		{"no command", nil, nil, cli.ExitOK},
		{"unknown command", nil, []string{"frobnicate"}, cli.ExitUsage},
		{"unknown flag", nil, []string{"ls", "--frob", "mem"}, cli.ExitUsage},
		{"bad flag value", nil, []string{"head", "-n", "x", "mem/bkt/k"}, cli.ExitUsage},
		{"missing argument", nil, []string{"ls"}, cli.ExitUsage},
		{"too many arguments", nil, []string{"pipe", "a", "b"}, cli.ExitUsage},
		{"unknown subcommand", nil, []string{"share", "frob"}, cli.ExitUsage},
		{"bad duration", nil, []string{"share", "download", "--expire", "soon", "mem/bkt/k"}, cli.ExitUsage},
		{"bad size", nil, []string{"find", "mem/bkt", "--larger", "big"}, cli.ExitUsage},
		{"unknown alias", nil, []string{"mb", "nope/bkt"}, cli.ExitNotFound},
		{"missing bucket", nil, []string{"ls", "mem/missing"}, cli.ExitNotFound},
		{"missing object", nil, []string{"cat", "mem/bkt/missing"}, cli.ExitNotFound},
		{"missing name", nil, []string{"ls", "mem/bkt/missing"}, cli.ExitNotFound},
		{"empty prefix", nil, []string{"ls", "mem/bkt/missing/"}, cli.ExitOK},
		{"missing local file", nil, []string{"cp", "missing", "mem/bkt/"}, cli.ExitNotFound},
		{"access denied", map[string]error{"List": domain.ErrAccessDenied}, []string{"ls", "mem/bkt"}, cli.ExitAccess},
		{"bad credentials", map[string]error{"ListBuckets": domain.ErrInvalidAccessKey}, []string{"ls", "mem"}, cli.ExitAccess},
		{"generic", map[string]error{"Get": errors.New("connection reset")}, []string{"cat", "mem/bkt/k"}, cli.ExitError},
		{"interrupted", map[string]error{"Get": context.Canceled}, []string{"cat", "mem/bkt/k"}, cli.ExitInterrupted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for m, err := range tc.fail {
				h.mem.FailOn(m, err)
			}
			defer func() {
				for m := range tc.fail {
					h.mem.FailOn(m, nil)
				}
			}()
			r := h.run(tc.args...)
			assert.Equal(t, tc.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
			if tc.code != 0 {
				assert.Contains(t, r.stderr, "nexoctl: error:")
			}
		})
	}
}

// covers: C-UX-01, TCL-E-19
func TestCompletion(t *testing.T) {
	h := newHarness(t)
	h.mustRun("alias", "set", "local", "http://local.test", "a", "b")
	h.mem.MakeBucket("photos")
	h.mem.MakeBucket("logs")
	h.mem.PutObject("photos", "2026/a.jpg", "a", clitest.ObjectSpec{})
	h.mem.PutObject("photos", "2025.txt", "b", clitest.ObjectSpec{})
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		r := h.mustRun("completion", shell)
		assert.Contains(t, r.stdout, "nexoctl", shell)
	}
	complete := func(args ...string) []string {
		r := h.mustRun(append([]string{"__complete"}, args...)...)
		var out []string
		for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
			if !strings.HasPrefix(line, ":") && line != "" {
				out = append(out, line)
			}
		}
		return out
	}
	assert.Equal(t, []string{"local/", "mem/"}, complete("ls", ""))
	assert.Equal(t, []string{"mem/"}, complete("cp", "x", "m"))
	assert.Equal(t, []string{"mem/logs/", "mem/photos/"}, complete("ls", "mem/"))
	assert.Equal(t, []string{"mem/photos/"}, complete("ls", "mem/ph"))
	assert.Equal(t, []string{"mem/photos/2025.txt", "mem/photos/2026/"}, complete("ls", "mem/photos/20"))
	assert.Equal(t, []string{"mem/photos/2026/a.jpg"}, complete("cat", "mem/photos/2026/"))
	assert.Equal(t, []string{"local", "mem"}, complete("alias", "remove", ""))
	assert.Equal(t, []string{"download"}, complete("anonymous", "set", "d"))
	assert.Empty(t, complete("ls", "zzz"), "no remote match: the shell completes files")
}

// covers: C-UX-01, TCL-U-03
func TestTerminalColorsAndProgress(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("bkt")
	h.write("a.bin", strings.Repeat("x", 2048))
	h.tty = true
	r := h.mustRun("ls", "mem")
	assert.Contains(t, r.stdout, "\x1b[34mbkt/\x1b[0m", "colors on a terminal")
	r = h.mustRun("ls", "--no-color", "mem")
	assert.NotContains(t, r.stdout, "\x1b[")
	h.env["NO_COLOR"] = "1"
	r = h.mustRun("ls", "mem")
	assert.NotContains(t, r.stdout, "\x1b[")
	delete(h.env, "NO_COLOR")

	r = h.mustRun("cp", "a.bin", "mem/bkt/")
	assert.Contains(t, r.stderr, "\r", "a progress bar on a terminal")
	assert.Contains(t, r.stderr, "100%")
	assert.NotContains(t, r.stdout, "`a.bin` ->", "the bar replaces the per-file lines")
	assert.Contains(t, r.stdout, "Copied 1 object (2.0KiB).")
	r = h.mustRun("cp", "--quiet", "a.bin", "mem/bkt/")
	assert.Empty(t, r.stderr, "--quiet hides the bar")
	assert.Empty(t, r.stdout)
	r = h.mustRun("cp", "--json", "a.bin", "mem/bkt/")
	assert.Empty(t, r.stderr, "--json hides the bar")
	h.tty = false
	r = h.mustRun("cp", "a.bin", "mem/bkt/")
	assert.Empty(t, r.stderr, "no bar without a terminal")
}

// covers: TCL-E-04
func TestInterruptedCommand(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("bkt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.stdin = "data"
	r := h.runContext(ctx, "pipe", "mem/bkt/k")
	assert.Equal(t, cli.ExitInterrupted, r.code)
	assert.Contains(t, r.stderr, "nexoctl: error: interrupted")
	_, ok := h.mem.Object("bkt", "k")
	assert.False(t, ok)
}

// covers: C-OBJ-06, TCL-E-07
func TestMirrorWatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file events are tested on Unix")
	}
	h := newHarness(t)
	h.mem.MakeBucket("bkt")
	h.write("src/a.txt", "a")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var r result
	wg.Add(1)
	go func() {
		defer wg.Done()
		r = h.runContext(ctx, "mirror", "--watch", "--remove", "--overwrite", "src", "mem/bkt/site")
	}()
	object := func(key string) (string, bool) { return h.mem.Object("bkt", key) }
	require.Eventually(t, func() bool { _, ok := object("site/a.txt"); return ok }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // the watch starts after the first pass

	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "src", "b.txt"), []byte("b"), 0o600))
	assert.Eventually(t, func() bool { v, ok := object("site/b.txt"); return ok && v == "b" }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "src", "a.txt"), []byte("a2"), 0o600))
	assert.Eventually(t, func() bool { v, _ := object("site/a.txt"); return v == "a2" }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.MkdirAll(filepath.Join(h.dir, "src", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "src", "sub", "c.txt"), []byte("c"), 0o600))
	assert.Eventually(t, func() bool { _, ok := object("site/sub/c.txt"); return ok }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Remove(filepath.Join(h.dir, "src", "b.txt")))
	assert.Eventually(t, func() bool { _, ok := object("site/b.txt"); return !ok }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.RemoveAll(filepath.Join(h.dir, "src", "sub")))
	assert.Eventually(t, func() bool { _, ok := object("site/sub/c.txt"); return !ok }, 5*time.Second, 10*time.Millisecond)

	cancel()
	wg.Wait()
	assert.Equal(t, 0, r.code, "Ctrl-C ends a watch normally: %s", r.stderr)
	assert.Contains(t, r.stdout, "Watching `src` for changes")
	assert.Contains(t, r.stdout, "Removed `mem/bkt/site/b.txt`.")
}

func TestNewRootCmdAndDefaults(t *testing.T) {
	root := commands.NewRootCmd(commands.Deps{})
	assert.Equal(t, "nexoctl", root.Name())
	deps := commands.DefaultDeps()
	assert.Equal(t, os.Stdout, deps.Stdout)

	// The default alias service is the JSON file under --config-dir.
	h := newHarness(t)
	dir := filepath.Join(h.dir, "custom")
	h.mustRun("alias", "set", "x", "http://x.test", "a", "b", "--config-dir", dir)
	st, err := os.Stat(filepath.Join(dir, cli.ConfigFileName))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}
	r := h.mustRun("alias", "list", "--config-dir", dir, "--json")
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	assert.Equal(t, "x", recs[0]["name"])

	// A corrupted configuration is a clear error (TCL-U-02).
	require.NoError(t, os.WriteFile(filepath.Join(dir, cli.ConfigFileName), []byte("{bad"), 0o600))
	r = h.run("alias", "list", "--config-dir", dir)
	assert.Equal(t, cli.ExitError, r.code)
	assert.Contains(t, r.stderr, "parse "+filepath.Join(dir, cli.ConfigFileName))

	failing := h.deps
	failing.Aliases = func() (cli.AliasService, error) { return nil, fmt.Errorf("no config") }
	failing.Stdout, failing.Stderr = io.Discard, io.Discard
	code := commands.Main(context.Background(), failing, []string{"alias", "list"})
	assert.Equal(t, cli.ExitError, code)
}

// covers: C-OBJ-03, C-OBJ-06 (regression: a key with ".." segments of a
// bucket of any S3-compatible endpoint wrote outside the destination of a
// recursive cp or of a mirror)
func TestUnsafeKeysNeverEscapeTheLocalDestination(t *testing.T) {
	h := newHarness(t)
	h.mem.PutObject("bkt", "../../escaped-cp.txt", "evil", clitest.ObjectSpec{})
	h.mem.PutObject("bkt", "a/../../../escaped-cp2.txt", "evil", clitest.ObjectSpec{})
	h.mem.PutObject("bkt", "ok.txt", "fine", clitest.ObjectSpec{})
	h.mem.PutObject("bkt2", "../../escaped-mirror.txt", "evil", clitest.ObjectSpec{})
	h.mem.PutObject("bkt2", "sub/ok.txt", "fine", clitest.ObjectSpec{})

	escaped := func(t *testing.T, rel string) {
		t.Helper()
		_, err := os.Stat(filepath.Join(h.dir, filepath.FromSlash(rel)))
		assert.ErrorIs(t, err, os.ErrNotExist, "%s written outside the destination", rel)
	}

	r := h.run("cp", "-r", "mem/bkt/", "a/b/out/")
	assert.Equal(t, cli.ExitError, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
	assert.Contains(t, r.stderr, "skipping `mem/bkt/../../escaped-cp.txt`")
	assert.Contains(t, r.stderr, "unsafe local file name")
	assert.Contains(t, r.stderr, "2 of 3 copy operations failed")
	escaped(t, "a/escaped-cp.txt")
	escaped(t, "escaped-cp2.txt")
	assert.Equal(t, "fine", h.read("a/b/out/ok.txt"), "the safe objects are still copied")

	r = h.run("mirror", "mem/bkt2", "m/n/dst")
	assert.Equal(t, cli.ExitError, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
	assert.Contains(t, r.stderr, "skipping `mem/bkt2/../../escaped-mirror.txt`")
	escaped(t, "m/escaped-mirror.txt")
	assert.Equal(t, "fine", h.read("m/n/dst/sub/ok.txt"))

	// JSON: one "error" line per rejected object on stderr.
	r = h.run("--json", "cp", "-r", "mem/bkt2/", "j/")
	assert.Equal(t, cli.ExitError, r.code)
	var rejected []string
	for _, l := range jsonLines(t, r.stderr) {
		if src, ok := l["source"].(string); ok {
			assert.Equal(t, "error", l["status"])
			assert.Contains(t, l["error"], "unsafe local file name")
			rejected = append(rejected, src)
		}
	}
	assert.Equal(t, []string{"mem/bkt2/../../escaped-mirror.txt"}, rejected)
}

// covers: TCL-E-01, C-CFG-01 (alias set verifies the credentials by
// default; regression: wrong credentials were saved with exit 0, and
// valid credentials without s3:ListAllMyBuckets were refused)
func TestAliasSetVerifiesCredentials(t *testing.T) {
	tests := []struct {
		name    string
		failure error
		args    []string
		code    int
		saved   bool
		calls   int
	}{
		{name: "valid", code: 0, saved: true, calls: 1},
		{name: "wrong secret", failure: fmt.Errorf("%w: SignatureDoesNotMatch", domain.ErrSignatureMismatch), code: cli.ExitAccess, calls: 1},
		{name: "unknown access key", failure: fmt.Errorf("%w: InvalidAccessKeyId", domain.ErrInvalidAccessKey), code: cli.ExitAccess, calls: 1},
		{name: "valid but may not list the buckets", failure: fmt.Errorf("%w: AccessDenied", domain.ErrAccessDenied), code: 0, saved: true, calls: 1},
		{name: "server unreachable", failure: errors.New("dial tcp: connection refused"), code: cli.ExitError, calls: 1},
		{name: "offline", failure: errors.New("dial tcp: connection refused"), args: []string{"--no-verify"}, code: 0, saved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			before := h.mem.Calls("ListBuckets")
			h.mem.FailOn("ListBuckets", tt.failure)
			r := h.run(append([]string{"alias", "set", "other", "http://s3.other.test", "AK", "SK"}, tt.args...)...)
			assert.Equal(t, tt.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
			assert.Equal(t, tt.calls, h.mem.Calls("ListBuckets")-before)
			list := h.mustRun("alias", "list")
			if tt.saved {
				assert.Contains(t, list.stdout, "other")
			} else {
				assert.NotContains(t, list.stdout, "other", "nothing is saved")
			}
		})
	}
	h := newHarness(t)
	r := h.run("alias", "set", "x", "http://s3.other.test", "AK", "SK", "--verify", "--no-verify")
	assert.Equal(t, cli.ExitUsage, r.code, r.stderr)
}

// covers: C-OBJ-07 (a raw download URL is signed only for an object that
// exists and that the alias may read; regression: the URL was printed with
// exit 0 and answered 403 or 404)
func TestRawDownloadURLsCheckTheObject(t *testing.T) {
	tests := []struct {
		name string
		args []string
		fail error
		code int
	}{
		{name: "share download readable", args: []string{"share", "download", "--raw", "mem/bkt/a.txt"}, code: 0},
		{name: "share download missing", args: []string{"share", "download", "--raw", "mem/bkt/nope.txt"}, code: cli.ExitNotFound},
		{name: "share download denied", args: []string{"share", "download", "--raw", "mem/bkt/a.txt"}, fail: fmt.Errorf("%w: AccessDenied", domain.ErrAccessDenied), code: cli.ExitAccess},
		{name: "presign GET missing version", args: []string{"presign", "--version-id", "v9", "mem/bkt/a.txt"}, code: cli.ExitNotFound},
		{name: "presign HEAD denied", args: []string{"presign", "-X", "HEAD", "mem/bkt/a.txt"}, fail: fmt.Errorf("%w: AccessDenied", domain.ErrAccessDenied), code: cli.ExitAccess},
		{name: "presign PUT is not checked", args: []string{"presign", "-X", "PUT", "mem/bkt/new.txt"}, fail: fmt.Errorf("%w: AccessDenied", domain.ErrAccessDenied), code: 0},
		{name: "presign DELETE is not checked", args: []string{"presign", "-X", "DELETE", "mem/bkt/nope.txt"}, code: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mem.MakeBucket("bkt")
			h.mem.PutObject("bkt", "a.txt", "A", clitest.ObjectSpec{})
			h.mem.FailOn("Stat", tt.fail)
			r := h.run(tt.args...)
			assert.Equal(t, tt.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
			if tt.code != 0 {
				assert.NotContains(t, r.stdout, "X-Amz-Signature", "no URL is printed")
			} else {
				assert.Contains(t, r.stdout, "X-Amz-Signature")
			}
		})
	}
}
