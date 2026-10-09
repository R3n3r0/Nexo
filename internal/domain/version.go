package domain

import (
	"strconv"
	"strings"
)

// CompatibleVersions reports whether two build versions may run in the
// same deployment: same major version, or a version that is not a release
// ("dev" builds, commit hashes) on either side. The bootstrap of a cluster
// and the internode authentication apply this one rule.
func CompatibleVersions(a, b string) bool {
	ma, okA := majorVersion(a)
	mb, okB := majorVersion(b)
	return !okA || !okB || ma == mb
}

// majorVersion parses the major of "vX.Y.Z" / "X.Y.Z".
func majorVersion(v string) (int, bool) {
	major, _, ok := strings.Cut(strings.TrimPrefix(v, "v"), ".")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(major)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
