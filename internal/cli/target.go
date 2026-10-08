package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Target is a command argument: a remote location "ALIAS[/BUCKET[/KEY]]"
// or a local path.
type Target struct {
	// Raw is the argument as written.
	Raw string
	// Alias, Bucket and Key describe a remote target. Key may be empty (the
	// bucket, or the alias when Bucket is empty too) or end with "/" (a
	// prefix).
	Alias  string
	Bucket string
	Key    string
	// Path is the local path (with "~" expanded) of a local target.
	Path string
}

// IsRemote reports whether the target names an alias.
func (t Target) IsRemote() bool { return t.Alias != "" }

// IsDirLike reports whether the target denotes a container rather than a
// single object or file: an alias, a bucket, a key or a path ending with a
// separator.
func (t Target) IsDirLike() bool {
	if t.IsRemote() {
		return t.Bucket == "" || t.Key == "" || strings.HasSuffix(t.Key, "/")
	}
	return strings.HasSuffix(t.Path, "/") || strings.HasSuffix(t.Path, string(filepath.Separator))
}

// String renders the target: "alias/bucket/key" or the local path.
func (t Target) String() string {
	if !t.IsRemote() {
		return t.Path
	}
	return JoinRemote(t.Alias, t.Bucket, t.Key)
}

// Child returns the remote target of key in the same bucket.
func (t Target) Child(key string) Target {
	return Target{Raw: JoinRemote(t.Alias, t.Bucket, key), Alias: t.Alias, Bucket: t.Bucket, Key: key}
}

// JoinRemote renders a remote location "alias[/bucket[/key]]".
func JoinRemote(alias, bucket, key string) string {
	switch {
	case bucket == "":
		return alias
	case key == "":
		return alias + "/" + bucket
	default:
		return alias + "/" + bucket + "/" + key
	}
}

// TargetParser parses the arguments of the commands (docs/06 TCL-U-01). An
// argument is remote when its first "/"-separated element is a known alias
// and it is not an explicit local path (absolute, "./", "../", "~", or a
// Windows drive or UNC path); everything else is a local path.
type TargetParser struct {
	// IsAlias reports whether a name is a configured alias.
	IsAlias func(name string) bool
	// Home is the home directory used to expand "~".
	Home string
	// Windows selects the Windows path rules ("\" separators, drive
	// letters, UNC paths); NewTargetParser sets it from the running OS.
	Windows bool
}

// NewTargetParser returns a parser for the running OS.
func NewTargetParser(isAlias func(string) bool, home string) TargetParser {
	return TargetParser{IsAlias: isAlias, Home: home, Windows: runtime.GOOS == "windows"}
}

// Parse parses one argument.
func (p TargetParser) Parse(arg string) (Target, error) {
	if arg == "" {
		return Target{}, UsageError("empty target")
	}
	if strings.ContainsRune(arg, 0) {
		return Target{}, UsageError("target %q contains a NUL byte", arg)
	}
	if p.isExplicitLocal(arg) {
		return Target{Raw: arg, Path: p.expandHome(arg)}, nil
	}
	first, rest, _ := strings.Cut(arg, "/")
	if p.IsAlias == nil || !p.IsAlias(first) {
		return Target{Raw: arg, Path: arg}, nil
	}
	t := Target{Raw: arg, Alias: first}
	if rest == "" {
		return t, nil
	}
	bucket, key, hasKey := strings.Cut(rest, "/")
	if bucket == "" {
		return Target{}, UsageError("target %q: empty bucket name", arg)
	}
	t.Bucket = bucket
	if hasKey {
		t.Key = key
	}
	return t, nil
}

// ParseAll parses several arguments.
func (p TargetParser) ParseAll(args []string) ([]Target, error) {
	out := make([]Target, 0, len(args))
	for _, a := range args {
		t, err := p.Parse(a)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// isExplicitLocal recognizes the arguments that are always local paths.
func (p TargetParser) isExplicitLocal(arg string) bool {
	if arg == "~" || arg == "." || arg == ".." || strings.HasPrefix(arg, "~/") ||
		strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") || strings.HasPrefix(arg, "/") {
		return true
	}
	if !p.Windows {
		return false
	}
	if strings.HasPrefix(arg, `~\`) || strings.HasPrefix(arg, `.\`) || strings.HasPrefix(arg, `..\`) || strings.HasPrefix(arg, `\`) {
		return true
	}
	// Drive letter: "C:", "C:\x", "C:/x".
	if len(arg) >= 2 && arg[1] == ':' && isASCIILetter(arg[0]) {
		return true
	}
	// A backslash before the first slash cannot be part of an alias name.
	if i := strings.IndexByte(arg, '\\'); i >= 0 {
		j := strings.IndexByte(arg, '/')
		return j < 0 || i < j
	}
	return false
}

// expandHome replaces a leading "~" with the home directory.
func (p TargetParser) expandHome(arg string) string {
	if p.Home == "" || !strings.HasPrefix(arg, "~") {
		return arg
	}
	rest := arg[1:]
	if rest == "" {
		return p.Home
	}
	if rest[0] == '/' || (p.Windows && rest[0] == '\\') {
		home := strings.TrimRight(p.Home, `/\`)
		return home + rest
	}
	return arg // "~user" is not supported: kept verbatim
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// HasGlob reports whether a local path contains glob metacharacters.
func HasGlob(path string) bool { return strings.ContainsAny(path, "*?[") }

// ExpandGlob expands a local glob pattern into the matching paths, sorted.
// A pattern without metacharacters is returned as is (even when the path
// does not exist); a pattern without matches is an error wrapping
// fs.ErrNotExist.
func ExpandGlob(pattern string) ([]string, error) {
	if !HasGlob(pattern) {
		return []string{pattern}, nil
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, UsageError("invalid pattern %q: %v", pattern, err)
	}
	if len(matches) == 0 {
		return nil, &os.PathError{Op: "glob", Path: pattern, Err: os.ErrNotExist}
	}
	sort.Strings(matches)
	return matches, nil
}
