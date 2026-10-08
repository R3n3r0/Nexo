package cli_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestSameEntryAndPlanMirror(t *testing.T) {
	tests := []struct {
		name     string
		src, dst cli.Entry
		same     bool
	}{
		{"local newer than remote", cli.Entry{Size: 1, ModTime: t0.Add(time.Minute)}, cli.Entry{Size: 1, ModTime: t0, ETag: "a"}, false},
		{"remote newer than local", cli.Entry{Size: 1, ModTime: t0}, cli.Entry{Size: 1, ModTime: t0.Add(time.Minute), ETag: "a"}, true},
		{"same second", cli.Entry{Size: 1, ModTime: t0.Add(900 * time.Millisecond)}, cli.Entry{Size: 1, ModTime: t0}, true},
		{"different size", cli.Entry{Size: 1, ModTime: t0}, cli.Entry{Size: 2, ModTime: t0.Add(time.Hour)}, false},
		{"same etag", cli.Entry{Size: 1, ETag: "x", ModTime: t0.Add(time.Hour)}, cli.Entry{Size: 1, ETag: "x", ModTime: t0}, true},
		{"different md5", cli.Entry{Size: 1, ETag: "x", ModTime: t0}, cli.Entry{Size: 1, ETag: "y", ModTime: t0.Add(time.Hour)}, false},
		{"multipart etag", cli.Entry{Size: 1, ETag: "x-2", ModTime: t0}, cli.Entry{Size: 1, ETag: "y", ModTime: t0.Add(time.Hour)}, true},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.same, cli.SameEntry(tc.src, tc.dst), tc.name)
	}

	src := map[string]cli.Entry{
		"new.txt":  {Rel: "new.txt", Size: 1, ModTime: t0},
		"same.txt": {Rel: "same.txt", Size: 2, ModTime: t0},
		"diff.txt": {Rel: "diff.txt", Size: 3, ModTime: t0},
	}
	dst := map[string]cli.Entry{
		"same.txt": {Rel: "same.txt", Size: 2, ModTime: t0.Add(time.Second)},
		"diff.txt": {Rel: "diff.txt", Size: 4, ModTime: t0},
		"old.txt":  {Rel: "old.txt", Size: 5, ModTime: t0},
	}
	kinds := func(actions []cli.MirrorAction) []string {
		var out []string
		for _, a := range actions {
			out = append(out, string(a.Kind)+":"+a.Rel)
		}
		return out
	}
	assert.Equal(t, []string{"conflict:diff.txt", "copy:new.txt"}, kinds(cli.PlanMirror(src, dst, cli.MirrorOptions{})))
	assert.Equal(t, []string{"overwrite:diff.txt", "copy:new.txt", "remove:old.txt"}, kinds(cli.PlanMirror(src, dst, cli.MirrorOptions{Overwrite: true, Remove: true})))
	assert.Empty(t, cli.PlanMirror(src, src, cli.MirrorOptions{Remove: true}), "identical sides: nothing to do")
}

func TestFindFilter(t *testing.T) {
	now := t0.Add(48 * time.Hour)
	e := func(rel string, size int64, age time.Duration) cli.Entry {
		return cli.Entry{Rel: rel, Size: size, ModTime: now.Add(-age)}
	}
	tests := []struct {
		name   string
		filter cli.FindFilter
		entry  cli.Entry
		match  bool
	}{
		{"all", cli.FindFilter{Larger: -1, Smaller: -1}, e("a/b.txt", 1, 0), true},
		{"name", cli.FindFilter{Name: "*.log", Larger: -1, Smaller: -1}, e("logs/app.log", 1, 0), true},
		{"name mismatch", cli.FindFilter{Name: "*.log", Larger: -1, Smaller: -1}, e("logs/app.txt", 1, 0), false},
		{"path", cli.FindFilter{Path: "logs/*.log", Larger: -1, Smaller: -1}, e("logs/app.log", 1, 0), true},
		{"path depth", cli.FindFilter{Path: "logs/*.log", Larger: -1, Smaller: -1}, e("logs/x/app.log", 1, 0), false},
		{"larger", cli.FindFilter{Larger: 10, Smaller: -1}, e("a", 11, 0), true},
		{"not larger", cli.FindFilter{Larger: 10, Smaller: -1}, e("a", 10, 0), false},
		{"smaller", cli.FindFilter{Larger: -1, Smaller: 10}, e("a", 9, 0), true},
		{"not smaller", cli.FindFilter{Larger: -1, Smaller: 10}, e("a", 10, 0), false},
		{"older", cli.FindFilter{Larger: -1, Smaller: -1, OlderThan: 24 * time.Hour, Now: now}, e("a", 1, 25*time.Hour), true},
		{"not older", cli.FindFilter{Larger: -1, Smaller: -1, OlderThan: 24 * time.Hour, Now: now}, e("a", 1, time.Hour), false},
		{"newer", cli.FindFilter{Larger: -1, Smaller: -1, NewerThan: 24 * time.Hour, Now: now}, e("a", 1, time.Hour), true},
		{"not newer", cli.FindFilter{Larger: -1, Smaller: -1, NewerThan: 24 * time.Hour, Now: now}, e("a", 1, 25*time.Hour), false},
		{"maxdepth", cli.FindFilter{Larger: -1, Smaller: -1, MaxDepth: 1}, e("a/b", 1, 0), false},
		{"maxdepth ok", cli.FindFilter{Larger: -1, Smaller: -1, MaxDepth: 2}, e("a/b", 1, 0), true},
	}
	for _, tc := range tests {
		require.NoError(t, tc.filter.Validate(), tc.name)
		assert.Equal(t, tc.match, tc.filter.Match(tc.entry), tc.name)
	}
	assert.ErrorIs(t, cli.FindFilter{Name: "[x"}.Validate(), cli.ErrUsage)
	assert.ErrorIs(t, cli.FindFilter{MaxDepth: -1}.Validate(), cli.ErrUsage)
}

func TestDiskUsage(t *testing.T) {
	du := cli.NewDiskUsage(2)
	for _, e := range []cli.Entry{
		{Rel: "top.txt", Size: 1},
		{Rel: "a/x.txt", Size: 10},
		{Rel: "a/b/y.txt", Size: 100},
		{Rel: "a/b/c/z.txt", Size: 1000},
		{Rel: "d/w.txt", Size: 10000},
	} {
		du.Add(e)
	}
	assert.Equal(t, []cli.UsageRow{
		{Prefix: "a/", Size: 1110, Objects: 3},
		{Prefix: "a/b/", Size: 1100, Objects: 2},
		{Prefix: "d/", Size: 10000, Objects: 1},
		{Prefix: "", Size: 11111, Objects: 5},
	}, du.Rows())
	total := cli.NewDiskUsage(0)
	total.Add(cli.Entry{Rel: "a/b", Size: 3})
	assert.Equal(t, []cli.UsageRow{{Size: 3, Objects: 1}}, total.Rows())
}

func TestBuildTree(t *testing.T) {
	rels := []string{"a/b/c.txt", "a/d.txt", "e.txt", "f/g/h.txt"}
	render := func(nodes []cli.TreeNode) string {
		var b strings.Builder
		for _, n := range nodes {
			b.WriteString(n.Indent + n.Name + "\n")
		}
		return b.String()
	}
	assert.Equal(t, `├─ a/
│  ├─ b/
│  │  └─ c.txt
│  └─ d.txt
├─ e.txt
└─ f/
   └─ g/
      └─ h.txt
`, render(cli.BuildTree(rels, false)))
	assert.Equal(t, `├─ a/
│  └─ b/
└─ f/
   └─ g/
`, render(cli.BuildTree(rels, true)))
	nodes := cli.BuildTree(rels, false)
	assert.Equal(t, cli.TreeNode{Path: "a/b/", Name: "b/", Dir: true, Depth: 2, Indent: "│  ├─ "}, nodes[1])
	assert.Empty(t, cli.BuildTree(nil, false))
}

func TestDiff(t *testing.T) {
	first := map[string]cli.Entry{
		"only1": {Rel: "only1", Size: 1},
		"size":  {Rel: "size", Size: 1},
		"etag":  {Rel: "etag", Size: 1, ETag: "a"},
		"mp":    {Rel: "mp", Size: 1, ETag: "a-2"},
		"same":  {Rel: "same", Size: 1, ETag: "x"},
	}
	second := map[string]cli.Entry{
		"only2": {Rel: "only2", Size: 1},
		"size":  {Rel: "size", Size: 2},
		"etag":  {Rel: "etag", Size: 1, ETag: "b"},
		"mp":    {Rel: "mp", Size: 1, ETag: "c"},
		"same":  {Rel: "same", Size: 1, ETag: "x"},
	}
	var got []string
	for _, d := range cli.Diff(first, second) {
		got = append(got, d.Symbol()+" "+d.Rel+" "+string(d.Kind))
	}
	assert.Equal(t, []string{"! etag etag", "< only1 only-in-first", "> only2 only-in-second", "! size size"}, got)
}

func TestShareSnippets(t *testing.T) {
	put := &cli.PresignedRequest{Method: "PUT", URL: "http://h/b/k?X-Amz-Signature=s&a='q'", Headers: map[string]string{"Content-Type": "text/plain"}}
	assert.Equal(t, `curl -X PUT -H 'Content-Type: text/plain' --upload-file <FILE> 'http://h/b/k?X-Amz-Signature=s&a='\''q'\'''`, cli.CurlCommand(put))
	assert.Equal(t, `curl -o <FILE> 'http://h/x'`, cli.CurlCommand(&cli.PresignedRequest{Method: "GET", URL: "http://h/x"}))
	assert.Equal(t, `curl -I 'http://h/x'`, cli.CurlCommand(&cli.PresignedRequest{Method: "HEAD", URL: "http://h/x"}))
	assert.Equal(t, `curl -X DELETE 'http://h/x'`, cli.CurlCommand(&cli.PresignedRequest{Method: "DELETE", URL: "http://h/x"}))
	form := &cli.PostPolicyForm{URL: "http://h/b", Fields: map[string]string{"key": "up/${filename}", "policy": "p\"<"}}
	assert.Equal(t, `curl --form-string 'key=up/${filename}' --form-string 'policy=p"<' -F file=@<FILE> 'http://h/b'`, cli.CurlPostCommand(form))
	// The fields are never read as local files ("@" or "<" with -F).
	at := &cli.PostPolicyForm{URL: "http://h/b", Fields: map[string]string{"key": "@inbox/${filename}", "x-amz-meta-note": "<notes.txt"}}
	assert.Equal(t, `curl --form-string 'key=@inbox/${filename}' --form-string 'x-amz-meta-note=<notes.txt' -F file=@<FILE> 'http://h/b'`, cli.CurlPostCommand(at))
	html := cli.HTMLPostForm(form)
	assert.Contains(t, html, `<form action="http://h/b" method="post" enctype="multipart/form-data">`)
	assert.Contains(t, html, `<input type="hidden" name="policy" value="p&#34;&lt;">`)
	assert.Contains(t, html, `<input type="file" name="file">`)
}
