package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
)

// covers: TCL-U-01
func TestTargetParserUnix(t *testing.T) {
	isAlias := func(name string) bool { return name == "local" || name == "prod" }
	p := cli.TargetParser{IsAlias: isAlias, Home: "/home/me"}
	tests := []struct {
		arg    string
		alias  string
		bucket string
		key    string
		path   string
		dir    bool
	}{
		{arg: "local", alias: "local", dir: true},
		{arg: "local/", alias: "local", dir: true},
		{arg: "local/photos", alias: "local", bucket: "photos", dir: true},
		{arg: "local/photos/", alias: "local", bucket: "photos", dir: true},
		{arg: "local/photos/2026/a.jpg", alias: "local", bucket: "photos", key: "2026/a.jpg"},
		{arg: "local/photos/2026/", alias: "local", bucket: "photos", key: "2026/", dir: true},
		{arg: "prod/b/with space/ü.txt", alias: "prod", bucket: "b", key: "with space/ü.txt"},
		{arg: "local/b//double", alias: "local", bucket: "b", key: "/double"},
		{arg: "unknown/b/k", path: "unknown/b/k"},
		{arg: "file.txt", path: "file.txt"},
		{arg: "./local/b", path: "./local/b"},
		{arg: "/abs/local", path: "/abs/local", dir: false},
		{arg: "../x/", path: "../x/", dir: true},
		{arg: "~", path: "/home/me"},
		{arg: "~/docs/", path: "/home/me/docs/", dir: true},
		{arg: "~other/docs", path: "~other/docs"},
		{arg: `local\b`, path: `local\b`},
	}
	for _, tc := range tests {
		t.Run(tc.arg, func(t *testing.T) {
			got, err := p.Parse(tc.arg)
			require.NoError(t, err)
			assert.Equal(t, tc.arg, got.Raw)
			assert.Equal(t, tc.alias, got.Alias)
			assert.Equal(t, tc.bucket, got.Bucket)
			assert.Equal(t, tc.key, got.Key)
			assert.Equal(t, tc.path, got.Path)
			assert.Equal(t, tc.alias != "", got.IsRemote())
			assert.Equal(t, tc.dir, got.IsDirLike())
		})
	}
}

// covers: TCL-U-01
func TestTargetParserWindows(t *testing.T) {
	p := cli.TargetParser{IsAlias: func(n string) bool { return n == "local" || n == "c" }, Home: `C:\Users\me`, Windows: true}
	tests := []struct {
		arg    string
		remote bool
		path   string
	}{
		{arg: `C:\data\file.txt`, path: `C:\data\file.txt`},
		{arg: `c:/data`, path: `c:/data`},
		{arg: `c/bucket/key`, remote: true},
		{arg: `\\server\share\x`, path: `\\server\share\x`},
		{arg: `.\local\b`, path: `.\local\b`},
		{arg: `local\b/c`, path: `local\b/c`},
		{arg: `local/b\c`, remote: true},
		{arg: `~\docs`, path: `C:\Users\me\docs`},
		{arg: `dir\`, path: `dir\`},
	}
	for _, tc := range tests {
		t.Run(tc.arg, func(t *testing.T) {
			got, err := p.Parse(tc.arg)
			require.NoError(t, err)
			assert.Equal(t, tc.remote, got.IsRemote())
			if !tc.remote {
				assert.Equal(t, tc.path, got.Path)
			}
		})
	}
}

func TestTargetParserErrors(t *testing.T) {
	p := cli.TargetParser{IsAlias: func(n string) bool { return n == "local" }}
	for _, arg := range []string{"", "local//key", "a\x00b"} {
		_, err := p.Parse(arg)
		require.Error(t, err, arg)
		assert.Equal(t, cli.ExitUsage, cli.ExitCode(err), arg)
	}
	all, err := p.ParseAll([]string{"local/b", "x"})
	require.NoError(t, err)
	require.Len(t, all, 2)
	_, err = p.ParseAll([]string{"local/b", ""})
	assert.Error(t, err)
}

func TestTargetHelpers(t *testing.T) {
	tg := cli.Target{Alias: "local", Bucket: "b", Key: "dir/"}
	assert.Equal(t, "local/b/dir/", tg.String())
	child := tg.Child("dir/x.txt")
	assert.Equal(t, "local/b/dir/x.txt", child.String())
	assert.Equal(t, "local/b/dir/x.txt", child.Raw)
	assert.Equal(t, "local", cli.JoinRemote("local", "", ""))
	assert.Equal(t, "local/b", cli.JoinRemote("local", "b", ""))
	assert.Equal(t, "file", cli.Target{Path: "file"}.String())
	assert.Equal(t, "", cli.DirPrefix(""))
	assert.Equal(t, "a/", cli.DirPrefix("a"))
	assert.Equal(t, "a/", cli.DirPrefix("a/"))
	joined, err := cli.JoinTarget(tg, "x")
	require.NoError(t, err)
	assert.Equal(t, "local/b/dir/x", joined.String())
	joined, err = cli.JoinTarget(tg, "../x")
	require.NoError(t, err, "a remote key is never a local path")
	assert.Equal(t, "local/b/dir/../x", joined.String())
	joined, err = cli.JoinTarget(cli.Target{Path: "out"}, "a/b")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("out", "a", "b"), joined.Path)
	_, err = cli.JoinTarget(cli.Target{Path: "out"}, "../../.bashrc")
	require.ErrorIs(t, err, cli.ErrUnsafeName)
	assert.Equal(t, cli.ExitError, cli.ExitCode(err))
}

// covers: C-OBJ-03, C-OBJ-06 (no remote key escapes a local destination)
func TestLocalName(t *testing.T) {
	tests := []struct {
		rel     string
		unix    bool // safe on Unix
		windows bool // safe on Windows
	}{
		{rel: "a.txt", unix: true, windows: true},
		{rel: "dir/sub/a.txt", unix: true, windows: true},
		{rel: "with space/ü.txt", unix: true, windows: true},
		{rel: "a//b", unix: true, windows: true},
		{rel: "..x/y..z", unix: true, windows: true},
		{rel: "..x/y..", unix: true},
		{rel: ""},
		{rel: "."},
		{rel: ".."},
		{rel: "../x"},
		{rel: "../../escaped.txt"},
		{rel: "a/../../x"},
		{rel: "a/../b"},
		{rel: "a/./b"},
		{rel: "dir/.."},
		{rel: "/etc/passwd"},
		{rel: "a\x00b"},
		{rel: `x\..\y`, unix: true},
		{rel: `..\x`, unix: true},
		{rel: `C:\Windows\x`, unix: true},
		{rel: "c:x", unix: true},
		{rel: "file.txt:stream", unix: true},
		{rel: "NUL", unix: true},
		{rel: "dir/com1.txt", unix: true},
		{rel: "lpt9", unix: true},
		{rel: "Con", unix: true},
		{rel: "trailing.", unix: true},
		{rel: "trailing ", unix: true},
		{rel: "console.txt", unix: true, windows: true},
		{rel: "com10", unix: true, windows: true},
	}
	for _, tc := range tests {
		t.Run(tc.rel, func(t *testing.T) {
			for _, sys := range []struct {
				windows bool
				safe    bool
			}{{false, tc.unix}, {true, tc.windows}} {
				name, err := cli.LocalNameOn(tc.rel, sys.windows)
				if sys.safe {
					require.NoError(t, err, "windows=%v", sys.windows)
					assert.NotEmpty(t, name)
					continue
				}
				require.ErrorIs(t, err, cli.ErrUnsafeName, "windows=%v", sys.windows)
				assert.Empty(t, name)
			}
		})
	}
}

// covers: TCL-U-01
func TestExpandGlob(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.log", "a.log", "c.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(n), 0o600))
	}
	got, err := cli.ExpandGlob(filepath.Join(dir, "*.log"))
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")}, got)

	plain := filepath.Join(dir, "missing.txt")
	got, err = cli.ExpandGlob(plain)
	require.NoError(t, err)
	assert.Equal(t, []string{plain}, got, "a path without metacharacters is kept as is")

	_, err = cli.ExpandGlob(filepath.Join(dir, "*.none"))
	assert.Equal(t, cli.ExitNotFound, cli.ExitCode(err))
	_, err = cli.ExpandGlob(filepath.Join(dir, "[x"))
	assert.Equal(t, cli.ExitUsage, cli.ExitCode(err))
	assert.True(t, cli.HasGlob("a*"))
	assert.False(t, cli.HasGlob("plain"))
}

func FuzzTargetParser(f *testing.F) {
	for _, s := range []string{"local/b/k", "~/x", `C:\x`, "local//", "a/b/c/d", "./x"} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, arg string, windows bool) {
		p := cli.TargetParser{IsAlias: func(n string) bool { return n == "local" }, Home: "/h", Windows: windows}
		got, err := p.Parse(arg)
		if err != nil {
			return
		}
		if got.IsRemote() {
			if got.String() != arg && got.String()+"/" != arg {
				t.Fatalf("remote target %q renders as %q", arg, got.String())
			}
			return
		}
		if got.Path == "" {
			t.Fatalf("local target %q without a path", arg)
		}
	})
}
