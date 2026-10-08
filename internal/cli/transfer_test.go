package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// fixture is an in-memory S3 store reachable through the alias "mem", plus
// a local directory.
type fixture struct {
	mem     *clitest.MemS3
	clock   *clock.Fake
	dir     string
	resolve cli.ClientResolver
	parser  cli.TargetParser
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(t0)
	mem := clitest.NewMemS3(clk)
	client := mem.Client(t)
	f := &fixture{mem: mem, clock: clk, dir: t.TempDir()}
	f.resolve = func(_ context.Context, alias string) (cli.S3Client, error) {
		if alias != "mem" {
			return nil, &cli.AliasNotFoundError{Name: alias}
		}
		return client, nil
	}
	f.parser = cli.TargetParser{IsAlias: func(n string) bool { return n == "mem" }}
	return f
}

func (f *fixture) target(t *testing.T, arg string) cli.Target {
	t.Helper()
	if !strings.HasPrefix(arg, "mem/") && arg != "mem" {
		dir := strings.HasSuffix(arg, "/")
		arg = filepath.Join(f.dir, arg)
		if dir {
			arg += string(filepath.Separator)
		}
	}
	tg, err := f.parser.Parse(arg)
	require.NoError(t, err)
	return tg
}

func (f *fixture) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(f.dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

func (f *fixture) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(b)
}

// plan returns the transfers as "source -> dest" with the local paths
// relative to the fixture directory.
func (f *fixture) plan(t *testing.T, sources []string, dest string, opts cli.PlanOptions) ([]string, error) {
	t.Helper()
	var srcs []cli.Target
	for _, s := range sources {
		srcs = append(srcs, f.target(t, s))
	}
	items, err := cli.PlanCopy(context.Background(), f.resolve, srcs, f.target(t, dest), opts)
	if err != nil {
		return nil, err
	}
	rel := func(tg cli.Target) string {
		if tg.IsRemote() {
			return tg.String()
		}
		r, err := filepath.Rel(f.dir, tg.Path)
		require.NoError(t, err)
		return filepath.ToSlash(r)
	}
	var out []string
	for _, it := range items {
		out = append(out, rel(it.Source)+" -> "+rel(it.Dest))
	}
	sort.Strings(out)
	return out, nil
}

// covers: C-OBJ-03
func TestPlanCopy(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.txt", "a")
	f.write(t, "dir/b.txt", "b")
	f.write(t, "dir/sub/c.txt", "c")
	require.NoError(t, os.MkdirAll(filepath.Join(f.dir, "existing"), 0o755))
	f.mem.PutObject("bkt", "p/x.txt", "x", clitest.ObjectSpec{})
	f.mem.PutObject("bkt", "p/q/y.txt", "y", clitest.ObjectSpec{})
	f.mem.PutObject("bkt", "p", "file named p", clitest.ObjectSpec{})

	tests := []struct {
		name    string
		sources []string
		dest    string
		opts    cli.PlanOptions
		want    []string
	}{
		{"file to key", []string{"a.txt"}, "mem/bkt/renamed.txt", cli.PlanOptions{}, []string{"a.txt -> mem/bkt/renamed.txt"}},
		{"file to prefix", []string{"a.txt"}, "mem/bkt/in/", cli.PlanOptions{}, []string{"a.txt -> mem/bkt/in/a.txt"}},
		{"file to bucket", []string{"a.txt"}, "mem/bkt", cli.PlanOptions{}, []string{"a.txt -> mem/bkt/a.txt"}},
		{"two files", []string{"a.txt", "dir/b.txt"}, "mem/bkt/two", cli.PlanOptions{}, []string{"a.txt -> mem/bkt/two/a.txt", "dir/b.txt -> mem/bkt/two/b.txt"}},
		{"dir itself", []string{"dir"}, "mem/bkt/", cli.PlanOptions{Recursive: true}, []string{"dir/b.txt -> mem/bkt/dir/b.txt", "dir/sub/c.txt -> mem/bkt/dir/sub/c.txt"}},
		{"dir contents", []string{"dir/"}, "mem/bkt/up", cli.PlanOptions{Recursive: true}, []string{"dir/b.txt -> mem/bkt/up/b.txt", "dir/sub/c.txt -> mem/bkt/up/sub/c.txt"}},
		{"object to local file", []string{"mem/bkt/p/x.txt"}, "out.txt", cli.PlanOptions{}, []string{"mem/bkt/p/x.txt -> out.txt"}},
		{"object to local dir", []string{"mem/bkt/p/x.txt"}, "existing", cli.PlanOptions{}, []string{"mem/bkt/p/x.txt -> existing/x.txt"}},
		{"prefix itself", []string{"mem/bkt/p"}, "out/", cli.PlanOptions{Recursive: true}, []string{"mem/bkt/p -> out/p", "mem/bkt/p/q/y.txt -> out/p/q/y.txt", "mem/bkt/p/x.txt -> out/p/x.txt"}},
		{"prefix contents", []string{"mem/bkt/p/"}, "out", cli.PlanOptions{Recursive: true}, []string{"mem/bkt/p/q/y.txt -> out/q/y.txt", "mem/bkt/p/x.txt -> out/x.txt"}},
		{"bucket itself", []string{"mem/bkt"}, "mem/other/", cli.PlanOptions{Recursive: true}, []string{"mem/bkt/p -> mem/other/bkt/p", "mem/bkt/p/q/y.txt -> mem/other/bkt/p/q/y.txt", "mem/bkt/p/x.txt -> mem/other/bkt/p/x.txt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.plan(t, tc.sources, tc.dest, tc.opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	failures := []struct {
		name    string
		sources []string
		dest    string
		opts    cli.PlanOptions
		code    int
	}{
		{"dir without -r", []string{"dir"}, "mem/bkt/", cli.PlanOptions{}, cli.ExitUsage},
		{"prefix without -r", []string{"mem/bkt/p/"}, "out", cli.PlanOptions{}, cli.ExitUsage},
		{"missing file", []string{"nope.txt"}, "mem/bkt/", cli.PlanOptions{}, cli.ExitNotFound},
		{"missing object", []string{"mem/bkt/nope"}, "out", cli.PlanOptions{}, cli.ExitNotFound},
		{"missing prefix", []string{"mem/bkt/nope/"}, "out", cli.PlanOptions{Recursive: true}, cli.ExitNotFound},
		{"alias only", []string{"a.txt"}, "mem", cli.PlanOptions{}, cli.ExitUsage},
		{"version of many", []string{"a.txt", "mem/bkt/p"}, "out/", cli.PlanOptions{VersionID: "v1"}, cli.ExitUsage},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.plan(t, tc.sources, tc.dest, tc.opts)
			require.Error(t, err)
			assert.Equal(t, tc.code, cli.ExitCode(err), err.Error())
		})
	}
}

// covers: C-OBJ-03 (a bucket of any S3 endpoint cannot choose where a
// recursive download writes: "zip slip")
func TestPlanCopyRejectsUnsafeNames(t *testing.T) {
	f := newFixture(t)
	for _, key := range []string{"p/ok.txt", "p/../x", "p/a/../../x", `p/x\..\y`, "p/.."} {
		f.mem.PutObject("bkt", key, key, clitest.ObjectSpec{})
	}
	for _, src := range []string{"mem/bkt/p/", "mem/bkt/p"} {
		t.Run(src, func(t *testing.T) {
			dest := f.target(t, "out/")
			items, err := cli.PlanCopy(context.Background(), f.resolve, []cli.Target{f.target(t, src)}, dest, cli.PlanOptions{Recursive: true})
			require.NoError(t, err)
			safe, unsafe := map[string]string{}, map[string]error{}
			for _, it := range items {
				if it.Err != nil {
					assert.Empty(t, it.Dest.Path, "a rejected item has no destination")
					unsafe[it.Source.Key] = it.Err
					continue
				}
				rel, err := filepath.Rel(filepath.Join(f.dir, "out"), it.Dest.Path)
				require.NoError(t, err)
				require.True(t, filepath.IsLocal(rel), "%s escapes the destination as %s", it.Source, it.Dest.Path)
				safe[it.Source.Key] = filepath.ToSlash(rel)
			}
			base := ""
			if !strings.HasSuffix(src, "/") {
				base = "p/"
			}
			want := map[string]string{"p/ok.txt": base + "ok.txt"}
			if runtime.GOOS != "windows" {
				// A backslash is an ordinary character of a Unix name.
				want[`p/x\..\y`] = base + `x\..\y`
			}
			assert.Equal(t, want, safe)
			for _, key := range []string{"p/../x", "p/a/../../x"} {
				require.ErrorIs(t, unsafe[key], cli.ErrUnsafeName, key)
			}
		})
	}

	// A single object named ".." under a prefix cannot become the parent
	// directory either.
	items, err := cli.PlanCopy(context.Background(), f.resolve, []cli.Target{f.target(t, "mem/bkt/p/..")}, f.target(t, "out/"), cli.PlanOptions{})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.ErrorIs(t, items[0].Err, cli.ErrUnsafeName)
}

// covers: C-OBJ-03, C-OBJ-04
func TestTransferCopyAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.mem.MakeBucket("bkt")
	f.mem.MakeBucket("dst")
	tr := cli.Transfer{Resolve: f.resolve}
	f.write(t, "page.html", "<p>hi</p>")

	// Upload: content type from the extension, metadata and tags.
	bar := cli.NewProgress(nil, 0, false, f.clock)
	res, err := tr.Copy(ctx, cli.CopyItem{Source: f.target(t, "page.html"), Dest: f.target(t, "mem/bkt/site/page.html")},
		cli.TransferOptions{Metadata: map[string]string{"owner": "me"}, Tags: map[string]string{"t": "1"}, Progress: bar})
	require.NoError(t, err)
	assert.Equal(t, int64(9), res.Size)
	assert.Equal(t, int64(9), bar.Done())
	client, _ := f.resolve(ctx, "mem")
	info, err := client.Stat(ctx, "bkt", "site/page.html", cli.StatOptions{})
	require.NoError(t, err)
	assert.Equal(t, "text/html; charset=utf-8", info.ContentType)
	assert.Equal(t, map[string]string{"owner": "me"}, info.Metadata)

	// Server-side copy within the alias keeps the metadata.
	_, err = tr.Copy(ctx, cli.CopyItem{Source: f.target(t, "mem/bkt/site/page.html"), Dest: f.target(t, "mem/dst/copy.html")}, cli.TransferOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, f.mem.Calls("Copy"))
	data, ok := f.mem.Object("dst", "copy.html")
	require.True(t, ok)
	assert.Equal(t, "<p>hi</p>", data)

	// Download: atomic write, modification time of the object.
	f.clock.Advance(time.Hour)
	_, err = tr.Copy(ctx, cli.CopyItem{Source: f.target(t, "mem/dst/copy.html"), Dest: f.target(t, "down/copy.html")}, cli.TransferOptions{})
	require.NoError(t, err)
	assert.Equal(t, "<p>hi</p>", f.read(t, "down/copy.html"))
	st, err := os.Stat(filepath.Join(f.dir, "down", "copy.html"))
	require.NoError(t, err)
	assert.True(t, st.ModTime().Equal(t0), "mtime %s", st.ModTime())
	entries, err := os.ReadDir(filepath.Join(f.dir, "down"))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file left")

	// Local copy keeps the modification time; a file onto itself fails.
	_, err = tr.Copy(ctx, cli.CopyItem{Source: f.target(t, "down/copy.html"), Dest: f.target(t, "local.html")}, cli.TransferOptions{})
	require.NoError(t, err)
	assert.Equal(t, "<p>hi</p>", f.read(t, "local.html"))
	_, err = tr.Copy(ctx, cli.CopyItem{Source: f.target(t, "local.html"), Dest: f.target(t, "local.html")}, cli.TransferOptions{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	// A failed download leaves no partial file.
	_, err = tr.Copy(ctx, cli.CopyItem{Source: f.target(t, "mem/dst/missing"), Dest: f.target(t, "down/missing")}, cli.TransferOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	_, err = os.Stat(filepath.Join(f.dir, "down", "missing"))
	assert.ErrorIs(t, err, os.ErrNotExist)

	// Upload of a stream.
	res, err = tr.Upload(ctx, strings.NewReader("streamed"), -1, f.target(t, "mem/bkt/stream"), cli.TransferOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(8), res.Size)

	// Delete a local file and a remote object.
	require.NoError(t, tr.Delete(ctx, f.target(t, "local.html"), ""))
	_, err = os.Stat(filepath.Join(f.dir, "local.html"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, tr.Delete(ctx, f.target(t, "mem/bkt/stream"), ""))
	_, ok = f.mem.Object("bkt", "stream")
	assert.False(t, ok)
	f.mem.FailOn("Remove", domain.ErrAccessDenied)
	assert.ErrorIs(t, tr.Delete(ctx, f.target(t, "mem/bkt/site/page.html"), ""), domain.ErrAccessDenied)
}

// covers: C-OBJ-06
func TestWalkAndCollect(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.write(t, "w/b.txt", "bb")
	f.write(t, "w/a-b/c.txt", "c")
	f.write(t, "w/a/d.txt", "d")
	got, err := cli.CollectEntries(cli.Walk(ctx, f.resolve, f.target(t, "w")))
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b/c.txt", "a/d.txt", "b.txt"}, cli.SortedKeys(got))
	assert.Equal(t, int64(2), got["b.txt"].Size)

	missing, err := cli.CollectEntries(cli.Walk(ctx, f.resolve, f.target(t, "nope")))
	require.NoError(t, err)
	assert.Empty(t, missing, "a missing directory is an empty listing")

	f.mem.PutObject("bkt", "p/x", "x", clitest.ObjectSpec{})
	f.mem.PutObject("bkt", "p/dir/", "", clitest.ObjectSpec{})
	f.mem.PutObject("bkt", "px", "not under p/", clitest.ObjectSpec{})
	remote, err := cli.CollectEntries(cli.Walk(ctx, f.resolve, f.target(t, "mem/bkt/p")))
	require.NoError(t, err)
	assert.Equal(t, []string{"x"}, cli.SortedKeys(remote), "placeholders skipped, key as a directory")

	_, err = cli.CollectEntries(cli.Walk(ctx, f.resolve, f.target(t, "mem")))
	assert.ErrorIs(t, err, cli.ErrUsage)
	_, err = cli.CollectEntries(cli.Walk(ctx, f.resolve, f.target(t, "mem/missing")))
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}
