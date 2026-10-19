package openh264

import (
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// build is a build of the library published by Cisco.
type build struct {
	name   string // file name, without the .bz2 of the download
	sha256 string // of the uncompressed file
}

// binaries are the Cisco builds of Version per GOOS/GOARCH. Cisco pays the
// MPEG LA royalties of H.264 for its builds when the end user downloads
// them from Cisco, which is why the worker downloads the library at its
// first start instead of shipping it.
func binaries() map[string]build {
	return map[string]build{
		"linux/amd64":   {"libopenh264-2.4.1-linux64.7.so", "1392d21466bc638e68151b716d5b2086d54cd812afd43253f1adb5b6e0185f51"},
		"linux/arm64":   {"libopenh264-2.4.1-linux-arm64.7.so", "e8ea7e42855ceb4a90e7bd0b3abeba0c58b5f97166e8b0a30eefd58e099557a4"},
		"darwin/amd64":  {"libopenh264-2.4.1-mac-x64.dylib", "cc0ba518a63791c37571f3c851f0aa03a4fbda5410acc214ecd4f24f8d1c478e"},
		"darwin/arm64":  {"libopenh264-2.4.1-mac-arm64.dylib", "213ff93831cfa3dd6d7ad0c3a3403a6ceedf4ac1341e1278b5b869d42fefb496"},
		"windows/amd64": {"openh264-2.4.1-win64.dll", "081b0c081480d177cbfddfbc90b1613640e702f875897b30d8de195cde73dd34"},
	}
}

// DownloadBase is where Cisco publishes its builds.
const DownloadBase = "https://ciscobinary.openh264.org/"

// maxLibrarySize bounds a download (the libraries are under 2 MiB).
const maxLibrarySize = 16 << 20

// ErrChecksum is returned when a library does not match its checksum.
var ErrChecksum = errors.New("openh264: checksum mismatch")

// Platform returns the file name and checksum of the library for this
// platform, and whether Cisco publishes one.
func Platform() (name, sum string, ok bool) {
	b, ok := binaries()[runtime.GOOS+"/"+runtime.GOARCH]
	return b.name, b.sha256, ok
}

// Fetch returns the path of the library of this platform in dir,
// downloading it from base (DownloadBase) when it is missing or does not
// match its pinned checksum.
func Fetch(ctx context.Context, client *http.Client, base, dir string) (string, error) {
	name, sum, ok := Platform()
	if !ok {
		return "", fmt.Errorf("openh264: no Cisco build for %s/%s: pass the path of the library", runtime.GOOS, runtime.GOARCH)
	}
	return fetch(ctx, client, base, dir, build{name: name, sha256: sum})
}

// fetch implements Fetch for the build b.
func fetch(ctx context.Context, client *http.Client, base, dir string, b build) (string, error) {
	name, sum := b.name, b.sha256
	path := filepath.Join(dir, name)
	switch err := verify(path, sum); {
	case err == nil:
		return path, nil
	case errors.Is(err, os.ErrNotExist), errors.Is(err, ErrChecksum):
		// Missing, or damaged: downloaded again.
	default:
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+name+".bz2", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req) // #nosec G107 G704 -- the Cisco download URL
	if err != nil {
		return "", fmt.Errorf("openh264: download %s: %w", req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openh264: download %s: %s", req.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(bzip2.NewReader(io.LimitReader(resp.Body, maxLibrarySize)), maxLibrarySize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("openh264: download %s: %w", req.URL, err)
	}
	if n > maxLibrarySize {
		return "", fmt.Errorf("openh264: download %s: larger than %d bytes", req.URL, maxLibrarySize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return "", fmt.Errorf("%w: %s is %s, expected %s", ErrChecksum, req.URL, got, sum)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// verify checks the checksum of the file at path.
func verify(path, sum string) error {
	f, err := os.Open(path) // #nosec G304 -- the library path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxLibrarySize+1)); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("%w: %s is %s, expected %s", ErrChecksum, path, got, sum)
	}
	return nil
}
