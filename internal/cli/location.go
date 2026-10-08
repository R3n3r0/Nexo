package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ErrUnsafeName reports a remote object name that cannot be written under
// a local directory without escaping it (the equivalent of a "zip slip"):
// an absolute name, a "." or ".." segment, a NUL byte or, on Windows, a
// backslash, a drive or stream separator or a reserved device name. Any
// S3-compatible endpoint (AWS S3 included) accepts such keys, so a bucket
// must never be able to choose where nexoctl writes. It wraps
// domain.ErrInvalidArgument.
var ErrUnsafeName = fmt.Errorf("unsafe local file name: %w", domain.ErrInvalidArgument)

// ClientResolver returns the S3 client of an alias.
type ClientResolver func(ctx context.Context, alias string) (S3Client, error)

// Entry is a file under a local directory or an object under a remote
// prefix, with its path relative to the root of the listing
// ("/"-separated).
type Entry struct {
	Rel     string
	Size    int64
	ModTime time.Time
	// ETag is set for remote objects only.
	ETag string
}

// DirPrefix returns the key prefix of a remote target used as a
// directory: "" (the bucket) or a key ending with "/".
func DirPrefix(key string) string {
	if key == "" || strings.HasSuffix(key, "/") {
		return key
	}
	return key + "/"
}

// Walk lists the regular files under a local directory, or the objects
// under a remote prefix (DirPrefix of the key), recursively and sorted by
// relative path. Directory placeholders (keys ending with "/") are
// skipped.
func Walk(ctx context.Context, resolve ClientResolver, t Target) iter.Seq2[Entry, error] {
	if !t.IsRemote() {
		return walkLocal(t.Path)
	}
	return func(yield func(Entry, error) bool) {
		if t.Bucket == "" {
			yield(Entry{}, UsageError("%s: a bucket is required", t))
			return
		}
		client, err := resolve(ctx, t.Alias)
		if err != nil {
			yield(Entry{}, err)
			return
		}
		prefix := DirPrefix(t.Key)
		for o, err := range client.List(ctx, t.Bucket, ListOptions{Prefix: prefix, Recursive: true}) {
			if err != nil {
				yield(Entry{}, err)
				return
			}
			if o.Prefix || strings.HasSuffix(o.Key, "/") {
				continue
			}
			if !yield(Entry{Rel: strings.TrimPrefix(o.Key, prefix), Size: o.Size, ModTime: o.LastModified, ETag: o.ETag}, nil) {
				return
			}
		}
	}
}

// walkLocal lists the regular files under root sorted by relative path; a
// missing root is an empty listing.
func walkLocal(root string) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		var entries []Entry
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root && errors.Is(err, fs.ErrNotExist) {
					return filepath.SkipAll
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			entries = append(entries, Entry{Rel: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime()})
			return nil
		})
		if err != nil {
			yield(Entry{}, fmt.Errorf("walk %s: %w", root, err))
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
		for _, e := range entries {
			if !yield(e, nil) {
				return
			}
		}
	}
}

// CollectEntries gathers a listing by relative path.
func CollectEntries(seq iter.Seq2[Entry, error]) (map[string]Entry, error) {
	out := map[string]Entry{}
	for e, err := range seq {
		if err != nil {
			return nil, err
		}
		out[e.Rel] = e
	}
	return out, nil
}

// JoinTarget returns the target of rel (a "/"-separated path relative to
// a listing) under a directory target. Under a local directory rel must be
// a safe local name (LocalName): a remote key such as "../../.bashrc" is
// rejected with ErrUnsafeName instead of resolving outside the directory.
func JoinTarget(dir Target, rel string) (Target, error) {
	if !dir.IsRemote() {
		name, err := LocalName(rel)
		if err != nil {
			return Target{}, err
		}
		p := filepath.Join(dir.Path, name)
		return Target{Raw: p, Path: p}, nil
	}
	key := DirPrefix(dir.Key) + rel
	return dir.Child(key), nil
}

// LocalName converts rel, a "/"-separated name taken from a remote listing
// (an object key relative to a prefix), into a relative path of the local
// system that stays inside the directory it is joined to. It rejects with
// ErrUnsafeName the empty name, absolute names, "." and ".." segments, NUL
// bytes and, on Windows, backslashes, ':' and the reserved device names
// (filepath.IsLocal).
func LocalName(rel string) (string, error) {
	return localName(rel, runtime.GOOS == "windows")
}

// localName is LocalName with the target system made explicit (tests).
func localName(rel string, windows bool) (string, error) {
	unsafe := func(why string) (string, error) {
		return "", fmt.Errorf("%w: %q %s", ErrUnsafeName, rel, why)
	}
	switch {
	case rel == "":
		return unsafe("is empty")
	case strings.ContainsRune(rel, 0):
		return unsafe("contains a NUL byte")
	case strings.HasPrefix(rel, "/"):
		return unsafe("is absolute")
	case windows && strings.ContainsAny(rel, `\:`):
		return unsafe(`contains '\' or ':'`)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "." || seg == ".." {
			return unsafe("has a '" + seg + "' segment")
		}
	}
	name := filepath.FromSlash(rel)
	if !filepath.IsLocal(name) {
		return unsafe("is not a local name")
	}
	if windows && !isLocalWindows(rel) {
		return unsafe("is a reserved name")
	}
	return name, nil
}

// isLocalWindows reports whether no segment of rel is a reserved device
// name of Windows (CON, PRN, AUX, NUL, CONIN$, CONOUT$, COM0-9, LPT0-9,
// also followed by an extension) or ends with a dot or a space, which
// Windows strips (so that "a." would alias "a").
func isLocalWindows(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return false
		}
		base := strings.ToUpper(seg)
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		base = strings.TrimRight(base, " ")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return false
		}
		if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
			return false
		}
	}
	return true
}

// isLocalDir reports whether path is an existing directory.
func isLocalDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
