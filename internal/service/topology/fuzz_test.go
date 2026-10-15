package topology

import (
	"errors"
	"strings"
	"testing"
)

// FuzzExpandArg checks the ellipsis expansion of the drive arguments
// (TS-U-01): no panic, every failure is ErrInvalidEndpoint,
// ErrInvalidEllipsis or ErrTooManyEndpoints, an expansion never exceeds
// the limit and its results carry no brace of an ellipsis.
func FuzzExpandArg(f *testing.F) {
	for _, seed := range []string{
		"/data{1...4}", "http://node{1...4}:9000/mnt/disk{01...16}", "/d{a...d}", "/d{A...z}", "/d{4...1}",
		"/d{1...}", "/d{...4}", "/d{1..4}", "/d}{", "/d{1...99999999999}", "/d{0...4294967295}", "",
		"http://n{1...2}:{9000...9001}/d{1...2}", "/d{01...1}",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, arg string) {
		if len(arg) > 512 {
			return
		}
		out, err := expandArg(arg)
		if err != nil {
			if !errors.Is(err, ErrInvalidEndpoint) && !errors.Is(err, ErrInvalidEllipsis) && !errors.Is(err, ErrTooManyEndpoints) {
				t.Fatalf("expandArg(%q): unexpected error %v", arg, err)
			}
			return
		}
		if len(out) == 0 || len(out) > maxExpansion {
			t.Fatalf("expandArg(%q): %d results", arg, len(out))
		}
		if hasEllipsis(arg) {
			for _, o := range out {
				if strings.Contains(o, "...") && hasEllipsis(o) {
					t.Fatalf("expandArg(%q): %q still has an ellipsis", arg, o)
				}
			}
		}
	})
}
