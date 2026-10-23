package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// stagedBinary is a binary downloaded and verified next to the running
// one.
type stagedBinary struct {
	path    string
	sha256  string
	version string
}

// Stage implements Agent: the binary is downloaded in the directory of the
// running one (a rename then swaps it, on the same file system), its
// SHA-256 verified and its version read by running it ("version --json").
func (s *service) Stage(ctx context.Context, in StageInput) (NodeState, error) {
	p, err := s.process()
	if err != nil {
		return NodeState{}, err
	}
	exe, err := p.Executable()
	if err != nil {
		return NodeState{}, err
	}
	sum := strings.ToLower(in.SHA256)
	if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
		return NodeState{}, fmt.Errorf("%w: update: invalid SHA-256 %q", domain.ErrInvalidArgument, in.SHA256)
	}
	path, err := s.download(ctx, in.URL, sum, exe)
	if err != nil {
		return NodeState{}, err
	}
	ver, err := probe(ctx, path)
	if err != nil {
		_ = os.Remove(path)
		return NodeState{}, err
	}
	s.mu.Lock()
	if s.staged != nil && s.staged.path != path {
		_ = os.Remove(s.staged.path)
	}
	s.staged = &stagedBinary{path: path, sha256: sum, version: ver}
	s.mu.Unlock()
	s.deps.Logger.InfoContext(ctx, "update: binary staged", "version", ver, "sha256", sum, "path", path)
	return s.State(ctx)
}

// download fetches the binary next to exe and verifies its digest.
func (s *service) download(ctx context.Context, rawURL, sum, exe string) (string, error) {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".nexo-download-*")
	if err != nil {
		return "", fmt.Errorf("%w: update: the directory of the binary (%s) is not writable by the server: install the binary in a directory "+
			"its user owns, or update it with the package manager: %v", domain.ErrPreconditionFailed, dir, err)
	}
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmp.Name())
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: update: %v", domain.ErrInvalidArgument, err)
	}
	resp, err := s.deps.HTTP.Do(req) //nolint:gosec // G107: the URL is given by an administrator (admin:ServerUpdate)
	if err != nil {
		return "", fmt.Errorf("update: download %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: update: download %s: %s", domain.ErrPreconditionFailed, rawURL, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, s.opts.maxSize+1))
	if err != nil {
		return "", fmt.Errorf("update: download %s: %w", rawURL, err)
	}
	if n > s.opts.maxSize {
		return "", fmt.Errorf("%w: update: the binary exceeds %d bytes", domain.ErrInvalidArgument, s.opts.maxSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return "", fmt.Errorf("%w: update: the SHA-256 of the binary is %s, not %s", domain.ErrInvalidArgument, got, sum)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // G302: an executable
		return "", err
	}
	path := filepath.Join(dir, ".nexo-update-"+sum[:16]+filepath.Ext(exe))
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

// probe runs the binary to read its version: it must be a Nexo server for
// this platform.
func probe(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "version", "--json") //nolint:gosec // G204: the binary staged and verified above
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: update: the binary does not run on this node (%s): %v %s",
			domain.ErrInvalidArgument, runtime.GOOS+"/"+runtime.GOARCH, err, strings.TrimSpace(stderr.String()))
	}
	var info struct {
		Version  string `json:"version"`
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil || info.Version == "" {
		return "", fmt.Errorf("%w: update: the binary is not a Nexo server (no version)", domain.ErrInvalidArgument)
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; info.Platform != "" && info.Platform != want {
		return "", fmt.Errorf("%w: update: the binary is built for %s, the node runs %s", domain.ErrInvalidArgument, info.Platform, want)
	}
	return info.Version, nil
}

// Activate implements Agent: the staged binary takes the place of the
// running one, which is kept as "<binary>.old", and the process is
// replaced once the answer is sent.
func (s *service) Activate(ctx context.Context, sum string) error {
	p, err := s.process()
	if err != nil {
		return err
	}
	exe, err := p.Executable()
	if err != nil {
		return err
	}
	s.mu.Lock()
	staged := s.staged
	s.mu.Unlock()
	if staged == nil || staged.sha256 != strings.ToLower(sum) {
		return fmt.Errorf("%w: update: no binary of SHA-256 %s is staged on node %s", domain.ErrPreconditionFailed, sum, s.deps.NodeID)
	}
	if err := swap(exe, staged.path); err != nil {
		return fmt.Errorf("update: install the binary: %w", err)
	}
	s.mu.Lock()
	s.staged = nil
	s.mu.Unlock()
	s.deps.Logger.InfoContext(ctx, "update: binary installed, restarting", "version", staged.version, "previous", exe+".old")
	return p.Exec()
}

// swap installs staged in place of exe, kept as exe.old; a failure leaves
// exe in place.
func swap(exe, staged string) error {
	if fi, err := os.Stat(exe); err == nil {
		_ = os.Chmod(staged, fi.Mode().Perm())
	}
	old := exe + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("%w (and the previous binary stays at %s: %v)", err, old, rerr)
		}
		return err
	}
	syncDir(filepath.Dir(exe))
	return nil
}

// syncDir makes the renames in dir durable (best effort: not on every
// platform).
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: the directory of the binary
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
