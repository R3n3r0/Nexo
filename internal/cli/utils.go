package cli

import (
	"fmt"
	"html"
	"path"
	"sort"
	"strings"
	"time"
)

// FindFilter selects the entries printed by find (docs/02 C-OBJ-08). The
// zero value matches everything; Larger and Smaller are disabled when
// negative.
type FindFilter struct {
	// Name is a glob matched against the base name ("*.log").
	Name string
	// Path is a glob matched against the relative path ("logs/*/*.gz").
	Path string
	// Larger and Smaller keep the entries strictly larger / smaller than
	// the size in bytes (-1: no limit).
	Larger  int64
	Smaller int64
	// OlderThan and NewerThan keep the entries modified before / after
	// Now minus the duration (0: no limit).
	OlderThan time.Duration
	NewerThan time.Duration
	// MaxDepth limits the depth of the relative path (0: unlimited; 1:
	// the entries directly under the target).
	MaxDepth int
	// Now is the reference time of the age filters.
	Now time.Time
}

// Validate checks the glob patterns.
func (f FindFilter) Validate() error {
	for _, p := range []string{f.Name, f.Path} {
		if p == "" {
			continue
		}
		if _, err := path.Match(p, ""); err != nil {
			return UsageError("invalid pattern %q: %v", p, err)
		}
	}
	if f.MaxDepth < 0 {
		return UsageError("--maxdepth must not be negative")
	}
	return nil
}

// Match reports whether an entry passes the filter.
func (f FindFilter) Match(e Entry) bool {
	if f.Name != "" {
		if ok, _ := path.Match(f.Name, path.Base(e.Rel)); !ok {
			return false
		}
	}
	if f.Path != "" {
		if ok, _ := path.Match(f.Path, e.Rel); !ok {
			return false
		}
	}
	if f.Larger >= 0 && e.Size <= f.Larger {
		return false
	}
	if f.Smaller >= 0 && e.Size >= f.Smaller {
		return false
	}
	if f.OlderThan > 0 && !e.ModTime.Before(f.Now.Add(-f.OlderThan)) {
		return false
	}
	if f.NewerThan > 0 && !e.ModTime.After(f.Now.Add(-f.NewerThan)) {
		return false
	}
	if f.MaxDepth > 0 && strings.Count(e.Rel, "/")+1 > f.MaxDepth {
		return false
	}
	return true
}

// UsageRow is the disk usage of a prefix ("" is the total).
type UsageRow struct {
	Prefix  string
	Size    int64
	Objects int64
}

// DiskUsage sums the sizes of a listing by directory, down to a depth
// (du, docs/02 C-OBJ-08).
type DiskUsage struct {
	depth int
	dirs  map[string]*UsageRow
	total UsageRow
}

// NewDiskUsage returns an accumulator reporting the directories down to
// depth (0: only the total).
func NewDiskUsage(depth int) *DiskUsage {
	return &DiskUsage{depth: depth, dirs: map[string]*UsageRow{}}
}

// Add accounts an entry.
func (d *DiskUsage) Add(e Entry) {
	d.total.Size += e.Size
	d.total.Objects++
	segments := strings.Split(e.Rel, "/")
	for level := 1; level <= d.depth && level < len(segments); level++ {
		dir := strings.Join(segments[:level], "/") + "/"
		row := d.dirs[dir]
		if row == nil {
			row = &UsageRow{Prefix: dir}
			d.dirs[dir] = row
		}
		row.Size += e.Size
		row.Objects++
	}
}

// Rows returns the directories sorted by path, then the total.
func (d *DiskUsage) Rows() []UsageRow {
	out := make([]UsageRow, 0, len(d.dirs)+1)
	for _, k := range SortedKeys(d.dirs) {
		out = append(out, *d.dirs[k])
	}
	return append(out, d.total)
}

// TreeNode is one line of a tree (tree, docs/02 C-OBJ-08).
type TreeNode struct {
	// Path is the relative path ("/"-terminated for directories).
	Path string
	// Name is the last element of the path.
	Name string
	Dir  bool
	// Depth is 1 for the entries directly under the root.
	Depth int
	// Indent is the drawing before the name ("│  ├─ ").
	Indent string
}

// BuildTree renders sorted relative file paths as a tree; dirsOnly keeps
// the directories.
func BuildTree(rels []string, dirsOnly bool) []TreeNode {
	type node struct {
		name     string
		children map[string]*node
		file     bool
	}
	root := &node{children: map[string]*node{}}
	for _, rel := range rels {
		cur := root
		segments := strings.Split(rel, "/")
		for i, s := range segments {
			if s == "" {
				continue
			}
			child := cur.children[s]
			if child == nil {
				child = &node{name: s, children: map[string]*node{}}
				cur.children[s] = child
			}
			if i == len(segments)-1 {
				child.file = true
			}
			cur = child
		}
	}
	var out []TreeNode
	var walk func(n *node, parent, indent string, depth int)
	walk = func(n *node, parent, indent string, depth int) {
		names := make([]string, 0, len(n.children))
		for name, c := range n.children {
			if dirsOnly && len(c.children) == 0 {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		for i, name := range names {
			c := n.children[name]
			last := i == len(names)-1
			branch, next := "├─ ", "│  "
			if last {
				branch, next = "└─ ", "   "
			}
			dir := len(c.children) > 0
			p := parent + name
			display := name
			if dir {
				p += "/"
				display += "/"
			}
			out = append(out, TreeNode{Path: p, Name: display, Dir: dir, Depth: depth, Indent: indent + branch})
			if dir {
				walk(c, p, indent+next, depth+1)
			}
		}
	}
	walk(root, "", "", 1)
	return out
}

// DiffKind classifies a difference reported by diff.
type DiffKind string

// Differences between two listings.
const (
	DiffOnlyFirst  DiffKind = "only-in-first"
	DiffOnlySecond DiffKind = "only-in-second"
	DiffSize       DiffKind = "size"
	DiffETag       DiffKind = "etag"
)

// DiffEntry is one difference between two listings.
type DiffEntry struct {
	Rel  string
	Kind DiffKind
	// First and Second are the entries of each side (nil when missing).
	First  *Entry
	Second *Entry
}

// Symbol is the marker of the human-readable diff: "<" only in the first,
// ">" only in the second, "!" different.
func (d DiffEntry) Symbol() string {
	switch d.Kind {
	case DiffOnlyFirst:
		return "<"
	case DiffOnlySecond:
		return ">"
	default:
		return "!"
	}
}

// Diff compares two listings by name, size and (when both sides have a
// comparable one) ETag, sorted by path (docs/02 C-OBJ-08).
func Diff(first, second map[string]Entry) []DiffEntry {
	var out []DiffEntry
	for rel, a := range first {
		b, ok := second[rel]
		switch {
		case !ok:
			out = append(out, DiffEntry{Rel: rel, Kind: DiffOnlyFirst, First: &a})
		case a.Size != b.Size:
			out = append(out, DiffEntry{Rel: rel, Kind: DiffSize, First: &a, Second: &b})
		case a.ETag != "" && b.ETag != "" && a.ETag != b.ETag && !isMultipartETag(a.ETag) && !isMultipartETag(b.ETag):
			out = append(out, DiffEntry{Rel: rel, Kind: DiffETag, First: &a, Second: &b})
		}
	}
	for rel, b := range second {
		if _, ok := first[rel]; !ok {
			out = append(out, DiffEntry{Rel: rel, Kind: DiffOnlySecond, Second: &b})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CurlCommand renders the curl command that uses a presigned request:
// PUT uploads <FILE> with the signed headers, the other methods print the
// response.
func CurlCommand(req *PresignedRequest) string {
	var b strings.Builder
	b.WriteString("curl")
	switch req.Method {
	case "GET":
		b.WriteString(" -o <FILE>")
	case "HEAD":
		b.WriteString(" -I")
	default:
		b.WriteString(" -X " + req.Method)
	}
	for _, k := range SortedKeys(req.Headers) {
		b.WriteString(" -H " + shellQuote(k+": "+req.Headers[k]))
	}
	if req.Method == "PUT" {
		b.WriteString(" --upload-file <FILE>")
	}
	b.WriteString(" " + shellQuote(req.URL))
	return b.String()
}

// CurlPostCommand renders the curl command of a POST policy upload: the
// form fields, then the file. The fields are sent with --form-string,
// verbatim: with -F a value starting with "@" or "<" (a key prefix
// "@inbox/") would make curl read a local file.
func CurlPostCommand(form *PostPolicyForm) string {
	var b strings.Builder
	b.WriteString("curl")
	for _, k := range SortedKeys(form.Fields) {
		b.WriteString(" --form-string " + shellQuote(k+"="+form.Fields[k]))
	}
	b.WriteString(" -F file=@<FILE> " + shellQuote(form.URL))
	return b.String()
}

// HTMLPostForm renders an HTML form that uploads a file with a POST
// policy.
func HTMLPostForm(form *PostPolicyForm) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<form action=\"%s\" method=\"post\" enctype=\"multipart/form-data\">\n", html.EscapeString(form.URL))
	for _, k := range SortedKeys(form.Fields) {
		fmt.Fprintf(&b, "  <input type=\"hidden\" name=\"%s\" value=\"%s\">\n", html.EscapeString(k), html.EscapeString(form.Fields[k]))
	}
	b.WriteString("  <input type=\"file\" name=\"file\">\n")
	b.WriteString("  <input type=\"submit\" value=\"Upload\">\n")
	b.WriteString("</form>")
	return b.String()
}
