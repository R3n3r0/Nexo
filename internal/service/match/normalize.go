package match

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// normalizeKey returns the canonical form of a decoded key: NFC when nfc
// is set, then the empty, "." and ".." segments resolved.
func normalizeKey(key string, nfc bool) string {
	if nfc {
		key = toNFC(key)
	}
	return cleanKey(key)
}

// toNFC returns s in the Unicode NFC form; s itself when it already is
// (the common case costs no allocation). The invalid UTF-8 bytes are
// kept as they are and only the valid runs between them are normalized:
// golang.org/x/text before v0.39.0 can loop forever on invalid input
// (GO-2026-5970), and an invalid byte is a normalization boundary anyway.
func toNFC(s string) string {
	if utf8.ValidString(s) {
		return nfcValid(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		n := validPrefix(s)
		b.WriteString(nfcValid(s[:n]))
		if n < len(s) {
			b.WriteByte(s[n])
			n++
		}
		s = s[n:]
	}
	return b.String()
}

// nfcValid returns the NFC form of a valid UTF-8 string.
func nfcValid(s string) string {
	if norm.NFC.IsNormalString(s) {
		return s
	}
	return norm.NFC.String(s)
}

// validPrefix returns the length of the longest valid UTF-8 prefix of s.
func validPrefix(s string) int {
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(s)
}

// cleanKey resolves the empty, "." and ".." segments of a key lexically:
// "a//b", "./a/b" and "x/../a/b" become "a/b". A ".." above the root is
// dropped. A leading and a trailing '/' are kept.
func cleanKey(key string) string {
	if !needsClean(key) {
		return key
	}
	leading := strings.HasPrefix(key, "/")
	trailing := strings.HasSuffix(key, "/") || strings.HasSuffix(key, "/.") || strings.HasSuffix(key, "/..") ||
		key == "." || key == ".."
	segments := strings.Split(key, "/")
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	var b strings.Builder
	b.Grow(len(key))
	if leading {
		b.WriteByte('/')
	}
	b.WriteString(strings.Join(out, "/"))
	if trailing && len(out) > 0 {
		b.WriteByte('/')
	}
	return b.String()
}

// needsClean reports whether a key has an empty, "." or ".." segment
// (besides the empty segments of a leading or a trailing '/').
func needsClean(key string) bool {
	if key == "." || key == ".." || strings.Contains(key, "//") {
		return true
	}
	for _, seg := range []string{"/./", "/../"} {
		if strings.Contains(key, seg) {
			return true
		}
	}
	return strings.HasPrefix(key, "./") || strings.HasPrefix(key, "../") ||
		strings.HasSuffix(key, "/.") || strings.HasSuffix(key, "/..")
}

// foldString maps every rune of s to the smallest rune of its Unicode
// simple case folding orbit: two strings that RE2 matches equal under
// (?i) fold to the same string. Invalid bytes are kept.
func foldString(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return foldASCII(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteRune(foldRune(r))
		i += size
	}
	return b.String()
}

// foldASCII folds an ASCII string. The orbits of 'k' and 's' contain
// non-ASCII runes (KELVIN SIGN, LONG S), but their smallest member is the
// ASCII capital letter, like for every other letter.
func foldASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'a' && b[j] <= 'z' {
					b[j] -= 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// foldRune returns the smallest rune of the simple case folding orbit of
// r.
func foldRune(r rune) rune {
	lo := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < lo {
			lo = f
		}
	}
	return lo
}
