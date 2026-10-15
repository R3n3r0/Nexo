//go:build upgrade

package upgrade_test

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The server binaries of the tests, set by TestMain: the release before
// the upgrade and the release under test.
var (
	oldBin string
	newBin string
	// builtVersions reports whether TestMain built both binaries, which
	// then announce oldVersion and newVersion.
	builtVersions bool
)

// The versions the binaries built by TestMain announce: the same major
// version, as for any release a rolling upgrade goes through (the
// bootstrap of a cluster refuses nodes of another major version).
const (
	oldVersion = "v0.7.0"
	newVersion = "v0.9.0"
)

// defaultFrom is the release before the one under test: the M7 commit
// (versioning, Object Lock, SSE/KMS, lifecycle, events), the oldest whose
// drives the releases of Nexo read.
const defaultFrom = "224bb31"

// modulePath is the import path of the module (the version is injected
// at link time, as by GoReleaser).
const modulePath = "github.com/r3n3r0/nexo"

func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" {
		fmt.Println("upgrade tests need Linux (signals, per-process /proc/self/cwd endpoints): skipped")
		os.Exit(0)
	}
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade tests:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run builds the binaries (unless NEXO_UPGRADE_OLD_BIN and
// NEXO_UPGRADE_NEW_BIN name them) and runs the tests.
func run(m *testing.M) (int, error) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return 0, err
	}
	dir, err := os.MkdirTemp("", "nexo-upgrade-bin-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	oldBin, newBin = os.Getenv("NEXO_UPGRADE_OLD_BIN"), os.Getenv("NEXO_UPGRADE_NEW_BIN")
	builtVersions = oldBin == "" && newBin == ""
	if oldBin == "" {
		ref := os.Getenv("NEXO_UPGRADE_FROM")
		if ref == "" {
			ref = defaultFrom
		}
		src := filepath.Join(dir, "src")
		if err := extract(root, ref, src); err != nil {
			return 0, fmt.Errorf("source of %s: %w", ref, err)
		}
		oldBin = filepath.Join(dir, "nexo-old")
		if err := build(src, oldBin, oldVersion); err != nil {
			return 0, fmt.Errorf("release %s: %w", ref, err)
		}
		// Only the binary is needed: the source tree is large.
		if err := os.RemoveAll(src); err != nil {
			return 0, err
		}
	}
	if newBin == "" {
		newBin = filepath.Join(dir, "nexo-new")
		if err := build(root, newBin, newVersion); err != nil {
			return 0, fmt.Errorf("release under test: %w", err)
		}
	}
	for _, p := range []*string{&oldBin, &newBin} {
		if *p, err = filepath.Abs(*p); err != nil {
			return 0, err
		}
	}
	return m.Run(), nil
}

// extract writes the tree of the commit ref of the repository root into
// dst (git archive: no worktree is registered, nothing is left behind).
func extract(root, ref, dst string) error {
	cmd := exec.Command("git", "archive", "--format=tar", ref)
	cmd.Dir = root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	untarErr := untar(out, dst)
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %w: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return untarErr
}

// untar extracts the directories and regular files of a tar stream into
// dst.
func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q outside of %s", h.Name, dst)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // a tree of the repository itself
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

// build builds the server of the module in dir into out, announcing
// version.
func build(dir, out, version string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-o", out,
		"-ldflags", "-X "+modulePath+"/internal/version.Version="+version, "./cmd/nexo")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, output)
	}
	return nil
}
